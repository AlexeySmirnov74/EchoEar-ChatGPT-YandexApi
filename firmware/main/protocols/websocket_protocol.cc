#include "websocket_protocol.h"
#include "board.h"
#include "system_info.h"
#include "application.h"
#include "settings.h"
#include "hello_features.h"
#include "endpoint_policy.h"
#include "deployment_endpoint.h"

#include <cstring>
#include <cJSON.h>
#include <esp_log.h>
#include <esp_timer.h>
#include <arpa/inet.h>
#include "assets/lang_config.h"

#define TAG "WS"

WebsocketProtocol::WebsocketProtocol(const std::atomic_bool* persistent_idle_enabled)
    : persistent_idle_enabled_(persistent_idle_enabled) {
    event_group_handle_ = xEventGroupCreate();
}

WebsocketProtocol::~WebsocketProtocol() {
    vEventGroupDelete(event_group_handle_);
}

bool WebsocketProtocol::Start() {
    // Only connect to server when audio channel is needed
    return true;
}

bool WebsocketProtocol::SendAudio(std::unique_ptr<AudioStreamPacket> packet) {
    if (traffic_suspended_.load()) return false;
    std::unique_lock<std::recursive_mutex> lock(websocket_mutex_, std::try_to_lock);
    if (!lock.owns_lock()) return false;
    if (traffic_suspended_.load()) return false;
    if (websocket_ == nullptr || !websocket_->IsConnected()) {
        return false;
    }

    if (version_ == 2) {
        std::string serialized;
        serialized.resize(sizeof(BinaryProtocol2) + packet->payload.size());
        auto bp2 = (BinaryProtocol2*)serialized.data();
        bp2->version = htons(version_);
        bp2->type = 0;
        bp2->reserved = 0;
        bp2->timestamp = htonl(packet->timestamp);
        bp2->payload_size = htonl(packet->payload.size());
        memcpy(bp2->payload, packet->payload.data(), packet->payload.size());

        return websocket_->Send(serialized.data(), serialized.size(), true);
    } else if (version_ == 3) {
        std::string serialized;
        serialized.resize(sizeof(BinaryProtocol3) + packet->payload.size());
        auto bp3 = (BinaryProtocol3*)serialized.data();
        bp3->type = 0;
        bp3->reserved = 0;
        bp3->payload_size = htons(packet->payload.size());
        memcpy(bp3->payload, packet->payload.data(), packet->payload.size());

        return websocket_->Send(serialized.data(), serialized.size(), true);
    } else {
        return websocket_->Send(packet->payload.data(), packet->payload.size(), true);
    }
}

bool WebsocketProtocol::SendText(const std::string& text) {
    if (traffic_suspended_.load()) return false;
    std::unique_lock<std::recursive_mutex> lock(websocket_mutex_, std::try_to_lock);
    if (!lock.owns_lock()) return false;
    if (traffic_suspended_.load()) return false;
    if (websocket_ == nullptr || !websocket_->IsConnected()) {
        return false;
    }

    if (!websocket_->Send(text)) {
        ESP_LOGE(TAG, "Failed to send text message");
        SetError(Lang::Strings::SERVER_ERROR);
        return false;
    }

    return true;
}

WebsocketProtocol::McpSendResult WebsocketProtocol::SendRestMcpReply(const std::string& payload, uint32_t epoch) {
    std::unique_lock<std::recursive_mutex> lock(websocket_mutex_, std::try_to_lock);
    if (!lock.owns_lock()) return McpSendResult::Busy;
    if (socket_generation_.load() != epoch || traffic_suspended_.load() ||
        !websocket_ || !websocket_->IsConnected()) return McpSendResult::Failed;
    const std::string message = "{\"session_id\":\"" + session_id_ + "\",\"type\":\"mcp\",\"payload\":" + payload + "}";
    // Failed writes have uncertain delivery and must not be retried.
    return websocket_->Send(message) ? McpSendResult::Sent : McpSendResult::Failed;
}

bool WebsocketProtocol::SendTextForImageEpoch(const std::string& text, uint32_t epoch) {
    std::unique_lock<std::recursive_mutex> lock(websocket_mutex_, std::try_to_lock);
    if (!lock.owns_lock() || socket_generation_.load() != epoch) return false;
    // The generation is checked under the same lock that protects socket
    // replacement, so an old queued ACK cannot escape on a newer connection.
    return SendText(text);
}

bool WebsocketProtocol::IsAudioChannelOpened() const {
    return TryGetAudioChannelState() == AudioChannelState::Open;
}

WebsocketProtocol::AudioChannelState WebsocketProtocol::TryGetAudioChannelState() const {
    std::unique_lock<std::recursive_mutex> lock(websocket_mutex_, std::try_to_lock);
    if (!lock.owns_lock()) return AudioChannelState::Busy;
    const auto last = ws_last_incoming_us_.load();
    const bool open = websocket_ != nullptr && websocket_->IsConnected() && !error_occurred_ &&
                      last > 0 && (esp_timer_get_time() - last) <= 120000000;
    return open ? AudioChannelState::Open : AudioChannelState::Closed;
}

void WebsocketProtocol::CloseAudioChannel() {
    std::lock_guard<std::recursive_mutex> lock(websocket_mutex_);
    ++socket_generation_;
    websocket_.reset();
}

bool WebsocketProtocol::OpenAudioChannel() {
    std::lock_guard<std::recursive_mutex> lock(websocket_mutex_);
    traffic_suspended_.store(false);
    const uint32_t generation = ++socket_generation_;
    persistent_idle_supported_.store(false);
    ws_last_incoming_us_.store(0);
    xEventGroupClearBits(event_group_handle_, WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT);
    Settings settings("websocket", false);
    std::string url = settings.GetString("url");
    std::string token = settings.GetString("token");
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    if (!endpoint_policy::AllowedWebsocket(GetDeploymentOtaUrl(), url)) {
        ESP_LOGW(TAG, "WebSocket endpoint rejected by deployment policy");
        return false;
    }
#endif
    int version = settings.GetInt("version");
    if (version != 0) {
        version_ = version;
    }

    error_occurred_ = false;

    auto network = Board::GetInstance().GetNetwork();
    websocket_ = network->CreateWebSocket(1);
    if (websocket_ == nullptr) {
        ESP_LOGE(TAG, "Failed to create websocket");
        return false;
    }

    if (!token.empty()) {
        // If token not has a space, add "Bearer " prefix
        if (token.find(" ") == std::string::npos) {
            token = "Bearer " + token;
        }
        websocket_->SetHeader("Authorization", token.c_str());
    }
    websocket_->SetHeader("Protocol-Version", std::to_string(version_).c_str());
    websocket_->SetHeader("Device-Id", SystemInfo::GetMacAddress().c_str());
    websocket_->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str());

    websocket_->OnData([this, generation](const char* data, size_t len, bool binary) {
        if (traffic_suspended_.load() || socket_generation_.load() != generation) return;
        ws_last_incoming_us_.store(esp_timer_get_time());
        if (binary) {
            if (on_incoming_audio_ != nullptr) {
                if (version_ == 2) {
                    if (len < sizeof(BinaryProtocol2)) return;
                    BinaryProtocol2* bp2 = (BinaryProtocol2*)data;
                    bp2->version = ntohs(bp2->version);
                    bp2->type = ntohs(bp2->type);
                    bp2->timestamp = ntohl(bp2->timestamp);
                    bp2->payload_size = ntohl(bp2->payload_size);
                    if (bp2->payload_size > len - sizeof(BinaryProtocol2)) return;
                    auto payload = (uint8_t*)bp2->payload;
                    on_incoming_audio_(std::make_unique<AudioStreamPacket>(AudioStreamPacket{
                        .sample_rate = server_sample_rate_,
                        .frame_duration = server_frame_duration_,
                        .timestamp = bp2->timestamp,
                        .payload = std::vector<uint8_t>(payload, payload + bp2->payload_size)
                    }));
                } else if (version_ == 3) {
                    if (len < sizeof(BinaryProtocol3)) return;
                    BinaryProtocol3* bp3 = (BinaryProtocol3*)data;
                    bp3->type = bp3->type;
                    bp3->payload_size = ntohs(bp3->payload_size);
                    if (bp3->payload_size > len - sizeof(BinaryProtocol3)) return;
                    auto payload = (uint8_t*)bp3->payload;
                    on_incoming_audio_(std::make_unique<AudioStreamPacket>(AudioStreamPacket{
                        .sample_rate = server_sample_rate_,
                        .frame_duration = server_frame_duration_,
                        .timestamp = 0,
                        .payload = std::vector<uint8_t>(payload, payload + bp3->payload_size)
                    }));
                } else {
                    on_incoming_audio_(std::make_unique<AudioStreamPacket>(AudioStreamPacket{
                        .sample_rate = server_sample_rate_,
                        .frame_duration = server_frame_duration_,
                        .timestamp = 0,
                        .payload = std::vector<uint8_t>((uint8_t*)data, (uint8_t*)data + len)
                    }));
                }
            }
        } else {
            // Parse JSON data
            auto root = cJSON_ParseWithLength(data, len);
            auto type = cJSON_GetObjectItem(root, "type");
            if (cJSON_IsString(type)) {
                if (strcmp(type->valuestring, "hello") == 0) {
                    ParseServerHello(root);
                    // Allow the application to validate and apply the optional
                    // clock sample after an authenticated reconnection.
                    if (on_incoming_json_ != nullptr) on_incoming_json_(root, generation);
                } else if (strcmp(type->valuestring, "heartbeat") == 0) {
                    // OnData refreshes the application-level idle watchdog below.
                } else {
                    if (on_incoming_json_ != nullptr) {
                        on_incoming_json_(root, generation);
                    }
                }
            } else {
                ESP_LOGE(TAG, "Invalid JSON or missing message type");
            }
            cJSON_Delete(root);
        }
    });

    websocket_->OnDisconnected([this, generation]() {
        if (socket_generation_.load() != generation) return;
        ESP_LOGI(TAG, "Websocket disconnected");
        if (on_audio_channel_closed_ != nullptr) {
            on_audio_channel_closed_();
        }
    });

    ESP_LOGI(TAG, "Connecting to websocket server with version: %d", version_);
    if (!websocket_->Connect(url.c_str())) {
        ESP_LOGE(TAG, "Failed to connect to websocket server, code=%d", websocket_->GetLastError());
        SetError(Lang::Strings::SERVER_NOT_CONNECTED);
        return false;
    }

    // Send hello message to describe the client
    auto message = GetHelloMessage();
    if (!SendText(message)) {
        return false;
    }

    // Wait for server hello
    EventBits_t bits = xEventGroupWaitBits(event_group_handle_, WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT, pdTRUE, pdFALSE, pdMS_TO_TICKS(10000));
    if (!(bits & WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT)) {
        ESP_LOGE(TAG, "Failed to receive server hello");
        SetError(Lang::Strings::SERVER_TIMEOUT);
        return false;
    }

    if (on_audio_channel_opened_ != nullptr) {
        on_audio_channel_opened_();
    }

    return true;
}

std::string WebsocketProtocol::GetHelloMessage() {
    // keys: message type, version, audio_params (format, sample_rate, channels)
    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "type", "hello");
    cJSON_AddNumberToObject(root, "version", version_);
    cJSON* features = cJSON_CreateObject();
    AddPersistentIdleHelloFeature(features, persistent_idle_enabled_);
#if CONFIG_USE_SERVER_AEC
    cJSON_AddBoolToObject(features, "aec", true);
#endif
    cJSON_AddBoolToObject(features, "mcp", true);
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    cJSON_AddBoolToObject(features, "image_preview", true);
    cJSON_AddBoolToObject(features, "image_background_v1", true);
#endif
    cJSON_AddItemToObject(root, "features", features);
    cJSON_AddStringToObject(root, "transport", "websocket");
    cJSON* audio_params = cJSON_CreateObject();
    cJSON_AddStringToObject(audio_params, "format", "opus");
    cJSON_AddNumberToObject(audio_params, "sample_rate", 16000);
    cJSON_AddNumberToObject(audio_params, "channels", 1);
    cJSON_AddNumberToObject(audio_params, "frame_duration", OPUS_FRAME_DURATION_MS);
    cJSON_AddItemToObject(root, "audio_params", audio_params);
    auto json_str = cJSON_PrintUnformatted(root);
    std::string message(json_str);
    cJSON_free(json_str);
    cJSON_Delete(root);
    return message;
}

void WebsocketProtocol::ParseServerHello(const cJSON* root) {
    auto transport = cJSON_GetObjectItem(root, "transport");
    if (!cJSON_IsString(transport) || strcmp(transport->valuestring, "websocket") != 0) {
        ESP_LOGE(TAG, "Unsupported or missing transport");
        return;
    }

    auto session_id = cJSON_GetObjectItem(root, "session_id");
    auto features = cJSON_GetObjectItem(root, "features");
    if (cJSON_IsObject(features)) {
        persistent_idle_supported_.store(cJSON_IsTrue(cJSON_GetObjectItem(features, "persistent_idle")));
    }
    if (cJSON_IsString(session_id)) {
        session_id_ = session_id->valuestring;
        ESP_LOGI(TAG, "Session ID: %s", session_id_.c_str());
    }

    auto audio_params = cJSON_GetObjectItem(root, "audio_params");
    if (cJSON_IsObject(audio_params)) {
        auto sample_rate = cJSON_GetObjectItem(audio_params, "sample_rate");
        if (cJSON_IsNumber(sample_rate)) {
            server_sample_rate_ = sample_rate->valueint;
        }
        auto frame_duration = cJSON_GetObjectItem(audio_params, "frame_duration");
        if (cJSON_IsNumber(frame_duration)) {
            server_frame_duration_ = frame_duration->valueint;
        }
    }

    xEventGroupSetBits(event_group_handle_, WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT);
}

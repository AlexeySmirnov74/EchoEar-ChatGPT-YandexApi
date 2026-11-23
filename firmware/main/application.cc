#include "application.h"
#include "rest_wake_sound.h"
#include "rest_policy.h"
#include "endpoint_policy.h"
#include "deployment_endpoint.h"
#include "legacy_mqtt_nvs.h"
#include <new>
#include "board.h"
#include "display.h"
#include "system_info.h"
#include "audio_codec.h"
#include "mqtt_protocol.h"
#include "websocket_protocol.h"
#include "assets/lang_config.h"
#include "mcp_server.h"
#include "assets.h"
#include "settings.h"

#include <cstring>
#include <ctime>
#include <cmath>
#include <sys/time.h>
#include <algorithm>
#include <nvs.h>
#include <esp_log.h>
#include <cJSON.h>
#include <driver/gpio.h>
#include <arpa/inet.h>
#include <font_awesome.h>
#include <wifi_station.h>
#include <esp_event.h>
#include <esp_wifi.h>
#include <esp_netif.h>
#include <esp_random.h>

#define TAG "Application"

#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
static bool RetryOldMqttCleanup(bool& started) {
    LegacyMqttNvs store;
    const auto result = legacy_cleanup::RemoveOldMqtt(store, started);
    switch (result) {
    case legacy_cleanup::Result::Removed: ESP_LOGI("LegacyMqtt", "cleanup=removed_verified"); return false;
    case legacy_cleanup::Result::Absent: ESP_LOGI("LegacyMqtt", "cleanup=already_absent_verified"); return false;
    case legacy_cleanup::Result::PreservedOther: ESP_LOGW("LegacyMqtt", "cleanup=other_endpoint_preserved"); return false;
    case legacy_cleanup::Result::Retry: ESP_LOGW("LegacyMqtt", "cleanup=retry_required"); return true;
    }
    return true;
}
#endif


static const char* const STATE_STRINGS[] = {
    "unknown",
    "starting",
    "configuring",
    "idle",
    "connecting",
    "listening",
    "speaking",
    "upgrading",
    "activating",
    "audio_testing",
    "fatal_error",
    "invalid_state"
};

Application::Application() {
    event_group_ = xEventGroupCreate();

#if CONFIG_USE_DEVICE_AEC && CONFIG_USE_SERVER_AEC
#error "CONFIG_USE_DEVICE_AEC and CONFIG_USE_SERVER_AEC cannot be enabled at the same time"
#elif CONFIG_USE_DEVICE_AEC
    aec_mode_ = kAecOnDeviceSide;
#elif CONFIG_USE_SERVER_AEC
    aec_mode_ = kAecOnServerSide;
#else
    aec_mode_ = kAecOff;
#endif

    esp_timer_create_args_t clock_timer_args = {
        .callback = [](void* arg) {
            Application* app = (Application*)arg;
            xEventGroupSetBits(app->event_group_, MAIN_EVENT_CLOCK_TICK);
        },
        .arg = this,
        .dispatch_method = ESP_TIMER_TASK,
        .name = "clock_timer",
        .skip_unhandled_events = true
    };
    esp_timer_create(&clock_timer_args, &clock_timer_handle_);
    esp_timer_create_args_t touch_timer_args = {
        .callback = [](void* arg) {
            auto* app = static_cast<Application*>(arg);
            app->Schedule([app]() {
                if (!app->touch_waiting_to_listen_.load()) return;
                if (Board::GetInstance().IsResting() || app->rest_waking_ || app->ringing_alarm_id_.load()) {
                    app->touch_waiting_to_listen_.store(false);
                    app->audio_service_.CancelPendingTouchFeedback();
                    return;
                }
                const auto remaining_ms = app->audio_service_.TouchFeedbackMuteRemainingMs();
                if (app->audio_service_.HasTouchFeedbackOutstanding() || remaining_ms) {
                    if (esp_timer_get_time() - app->touch_waiting_since_us_ >= 1000000) {
                        // Queued tones can be cancelled atomically. If a block
                        // is already in the hardware, never claim readiness.
                        const bool safe = app->audio_service_.CancelPendingTouchFeedback();
                        app->touch_waiting_to_listen_.store(false);
                        if (safe && !remaining_ms && !app->touch_feedback_started_.load() && !app->ringing_alarm_id_.load() &&
                            app->device_state_ == kDeviceStateIdle) {
                            app->ToggleChatState();
                        } else {
                            ESP_LOGE(TAG, "Touch feedback output stalled; listening not started");
                            Board::GetInstance().GetDisplay()->ShowNotification("Не удалось включить звук");
                        }
                        return;
                    }
                    // Another recognised tap is still being played.
                    const int64_t left_us = 1000000 - (esp_timer_get_time() - app->touch_waiting_since_us_);
                    esp_timer_start_once(app->touch_ready_timer_handle_, std::min<int64_t>(left_us, std::max<uint32_t>(20, remaining_ms) * 1000));
                    return;
                }
                app->touch_waiting_to_listen_.store(false);
                if (!app->ringing_alarm_id_.load() && app->device_state_ == kDeviceStateIdle)
                    app->ToggleChatState();
            });
        },
        .arg = this,
        .dispatch_method = ESP_TIMER_TASK,
        .name = "touch_ready",
        .skip_unhandled_events = true
    };
    esp_timer_create(&touch_timer_args, &touch_ready_timer_handle_);
}

Application::~Application() {
    if (touch_ready_timer_handle_ != nullptr) {
        esp_timer_stop(touch_ready_timer_handle_);
        esp_timer_delete(touch_ready_timer_handle_);
    }
    if (clock_timer_handle_ != nullptr) {
        esp_timer_stop(clock_timer_handle_);
        esp_timer_delete(clock_timer_handle_);
    }
    vEventGroupDelete(event_group_);
}

void Application::CheckAssetsVersion() {
    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    auto& assets = Assets::GetInstance();

    if (!assets.partition_valid()) {
        ESP_LOGW(TAG, "Assets partition is disabled for board %s", BOARD_NAME);
        return;
    }
    
    Settings settings("assets", true);
    // Check if there is a new assets need to be downloaded
    std::string download_url = settings.GetString("download_url");

    if (!download_url.empty()) {
        settings.EraseKey("download_url");

        char message[256];
        snprintf(message, sizeof(message), Lang::Strings::FOUND_NEW_ASSETS, download_url.c_str());
        Alert(Lang::Strings::LOADING_ASSETS, message, "cloud_arrow_down", Lang::Sounds::OGG_UPGRADE);
        
        // Wait for the audio service to be idle for 3 seconds
        vTaskDelay(pdMS_TO_TICKS(3000));
        SetDeviceState(kDeviceStateUpgrading);
        board.SetPowerSaveMode(false);
        display->SetChatMessage("system", Lang::Strings::PLEASE_WAIT);

        bool success = assets.Download(download_url, [display](int progress, size_t speed) -> void {
            std::thread([display, progress, speed]() {
                char buffer[32];
                snprintf(buffer, sizeof(buffer), "%d%% %uKB/s", progress, speed / 1024);
                display->SetChatMessage("system", buffer);
            }).detach();
        });

        board.SetPowerSaveMode(true);
        vTaskDelay(pdMS_TO_TICKS(1000));

        if (!success) {
            Alert(Lang::Strings::ERROR, Lang::Strings::DOWNLOAD_ASSETS_FAILED, "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
            vTaskDelay(pdMS_TO_TICKS(2000));
            return;
        }
    }

    // Apply assets
    assets.Apply();
    display->SetChatMessage("system", "");
    display->SetEmotion("microchip_ai");
}

void Application::CheckNewVersion(Ota& ota) {
    const int MAX_RETRY = 10;
    int retry_count = 0;
    int retry_delay = 10; // 初始重试延迟为10秒

    auto& board = Board::GetInstance();
    while (true) {
        SetDeviceState(kDeviceStateActivating);
        auto display = board.GetDisplay();
        display->SetStatus(Lang::Strings::CHECKING_NEW_VERSION);

        esp_err_t err = ota.CheckVersion();
        if (err != ESP_OK) {
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
            // Keep alarms and other local controls available; the persistent
            // worker will retry the trusted OTA service in the background.
            ESP_LOGW(TAG, "OTA unavailable; continuing with validated cached connection");
            return;
#endif
            retry_count++;
            if (retry_count >= MAX_RETRY) {
                ESP_LOGE(TAG, "Too many retries, exit version check");
                return;
            }

            char error_message[128];
            snprintf(error_message, sizeof(error_message), "code=%d", err);
            char buffer[256];
            snprintf(buffer, sizeof(buffer), Lang::Strings::CHECK_NEW_VERSION_FAILED, retry_delay, error_message);
            Alert(Lang::Strings::ERROR, buffer, "cloud_slash", Lang::Sounds::OGG_EXCLAMATION);

            ESP_LOGW(TAG, "Check new version failed, retry in %d seconds (%d/%d)", retry_delay, retry_count, MAX_RETRY);
            for (int i = 0; i < retry_delay; i++) {
                vTaskDelay(pdMS_TO_TICKS(1000));
                if (device_state_ == kDeviceStateIdle) {
                    break;
                }
            }
            retry_delay *= 2; // 每次重试后延迟时间翻倍
            continue;
        }
        retry_count = 0;
        retry_delay = 10; // 重置重试延迟时间

        if (ota.HasNewVersion()) {
            if (UpgradeFirmware(ota)) {
                return; // This line will never be reached after reboot
            }
            // If upgrade failed, continue to normal operation (don't break, just fall through)
        }

        // No new version, mark the current version as valid
        ota.MarkCurrentVersionValid();
        if (!ota.HasActivationCode() && !ota.HasActivationChallenge()) {
            xEventGroupSetBits(event_group_, MAIN_EVENT_CHECK_NEW_VERSION_DONE);
            // Exit the loop if done checking new version
            break;
        }

        display->SetStatus(Lang::Strings::ACTIVATION);
        // Activation code is shown to the user and waiting for the user to input
        if (ota.HasActivationCode()) {
            ShowActivationCode(ota.GetActivationCode(), ota.GetActivationMessage());
        }

        // This will block the loop until the activation is done or timeout
        for (int i = 0; i < 10; ++i) {
            ESP_LOGI(TAG, "Activating... %d/%d", i + 1, 10);
            esp_err_t err = ota.Activate();
            if (err == ESP_OK) {
                xEventGroupSetBits(event_group_, MAIN_EVENT_CHECK_NEW_VERSION_DONE);
                break;
            } else if (err == ESP_ERR_TIMEOUT) {
                vTaskDelay(pdMS_TO_TICKS(3000));
            } else {
                vTaskDelay(pdMS_TO_TICKS(10000));
            }
            if (device_state_ == kDeviceStateIdle) {
                break;
            }
        }
    }
}

void Application::ShowActivationCode(const std::string& code, const std::string& message) {
    struct digit_sound {
        char digit;
        const std::string_view& sound;
    };
    static const std::array<digit_sound, 10> digit_sounds{{
        digit_sound{'0', Lang::Sounds::OGG_0},
        digit_sound{'1', Lang::Sounds::OGG_1}, 
        digit_sound{'2', Lang::Sounds::OGG_2},
        digit_sound{'3', Lang::Sounds::OGG_3},
        digit_sound{'4', Lang::Sounds::OGG_4},
        digit_sound{'5', Lang::Sounds::OGG_5},
        digit_sound{'6', Lang::Sounds::OGG_6},
        digit_sound{'7', Lang::Sounds::OGG_7},
        digit_sound{'8', Lang::Sounds::OGG_8},
        digit_sound{'9', Lang::Sounds::OGG_9}
    }};

    // This sentence uses 9KB of SRAM, so we need to wait for it to finish
    Alert(Lang::Strings::ACTIVATION, message.c_str(), "link", Lang::Sounds::OGG_ACTIVATION);

    for (const auto& digit : code) {
        auto it = std::find_if(digit_sounds.begin(), digit_sounds.end(),
            [digit](const digit_sound& ds) { return ds.digit == digit; });
        if (it != digit_sounds.end()) {
            audio_service_.PlaySound(it->sound);
        }
    }
}

void Application::Alert(const char* status, const char* message, const char* emotion, const std::string_view& sound) {
    ESP_LOGW(TAG, "Alert [%s] %s: %s", emotion, status, message);
    auto display = Board::GetInstance().GetDisplay();
    display->SetStatus(status);
    display->SetEmotion(emotion);
    display->SetChatMessage("system", message);
    if (!sound.empty()) {
        audio_service_.PlaySound(sound);
    }
}

void Application::DismissAlert() {
    if (device_state_ == kDeviceStateIdle) {
        auto display = Board::GetInstance().GetDisplay();
        display->SetStatus(Lang::Strings::STANDBY);
        display->SetEmotion("neutral");
        display->SetChatMessage("system", "");
    }
}

void Application::HandleEchoEarTouch() {
    if (!ringing_alarm_id_.load() && !Board::GetInstance().IsResting() && !rest_waking_ && device_state_ == kDeviceStateIdle && touch_ready_timer_handle_) {
        Schedule([this]() {
            if (ringing_alarm_id_.load() || Board::GetInstance().IsResting() || rest_waking_ || device_state_ != kDeviceStateIdle) {
                RequestTouchFeedback();
                return;
            }
            if (!touch_waiting_to_listen_.load()) {
                touch_waiting_since_us_ = esp_timer_get_time();
                touch_feedback_started_.store(false);
                touch_waiting_to_listen_.store(true);
            }
            // Watchdog for a delayed output worker. On actual cue start the
            // audio callback re-arms the precise echo-mask interval.
            esp_timer_stop(touch_ready_timer_handle_);
            const int64_t left_us = std::max<int64_t>(1, 1000000 - (esp_timer_get_time() - touch_waiting_since_us_));
            esp_timer_start_once(touch_ready_timer_handle_, std::min<int64_t>(800000, left_us));
            if (!audio_service_.RequestTouchFeedback()) {
                // Keep waiting for the bounded queue to drain; never start a
                // microphone while old feedback could still be emitted.
                ESP_LOGW(TAG, "Touch feedback queue full");
            }
        });
        return;
    }
    ToggleChatState();
    Schedule([this]() { RequestTouchFeedback(); });
}

void Application::ToggleChatState() {
    ESP_LOGI("AlarmDiag", "touch ringing_id=%lu state=%d", static_cast<unsigned long>(ringing_alarm_id_.load()), static_cast<int>(device_state_));
    if (IsAlarmRinging()) {
        Schedule([this]() { DismissAlarmForTouch(); });
        return;
    }
    if (rest_waking_) return;
    if (device_state_ == kDeviceStateActivating) {
        SetDeviceState(kDeviceStateIdle);
        return;
    } else if (device_state_ == kDeviceStateWifiConfiguring) {
        audio_service_.EnableAudioTesting(true);
        SetDeviceState(kDeviceStateAudioTesting);
        return;
    } else if (device_state_ == kDeviceStateAudioTesting) {
        audio_service_.EnableAudioTesting(false);
        SetDeviceState(kDeviceStateWifiConfiguring);
        return;
    }

    if (!protocol_) {
        ESP_LOGE(TAG, "Protocol not initialized");
        return;
    }

    if (device_state_ == kDeviceStateIdle) {
        Schedule([this]() {
            if (DismissAlarmForTouch()) return;
            if (device_state_ != kDeviceStateIdle) return;
            if (persistent_protocol_ && persistent_connecting_.load() && !persistent_link_enabled_) return;
            if (persistent_link_enabled_) {
                if (persistent_connecting_.load() || NetworkGenerationChanged(persistent_network_generation_.load(), persistent_network_handled_) || !protocol_->IsAudioChannelOpened()) {
                    persistent_pending_listen_ = true;
                    persistent_pending_mode_ = aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime;
                    persistent_retry_.RetryNow();
                    SetDeviceState(kDeviceStateConnecting);
                    StartPersistentConnection();
                } else {
                    SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
                }
                return;
            }
            if (!protocol_->IsAudioChannelOpened()) {
                SetDeviceState(kDeviceStateConnecting);
                if (!protocol_->OpenAudioChannel()) {
                    return;
                }
            }

            SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
        });
    } else if (device_state_ == kDeviceStateSpeaking) {
        Schedule([this]() {
            if (DismissAlarmForTouch()) return;
            AbortSpeaking(kAbortReasonNone);
        });
    } else if (device_state_ == kDeviceStateListening) {
        Schedule([this]() {
            if (DismissAlarmForTouch()) return;
            if (persistent_link_enabled_) {
                protocol_->SendPauseListening(persistent_listen_id_);
                if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
                SetDeviceState(kDeviceStateIdle);
                audio_service_.ResetDecoder();
            } else {
                if (persistent_protocol_) StartPersistentClose();
                else protocol_->CloseAudioChannel();
            }
        });
    }
}

bool Application::DismissAlarmForTouch() {
    uint32_t id = ringing_alarm_id_.load();
    if (!id) return false;
    ESP_LOGI("AlarmDiag", "dismiss_attempt id=%lu now=%lu", static_cast<unsigned long>(id), static_cast<unsigned long>(static_cast<uint32_t>(time(nullptr))));
    if (alarm_store_.Dismiss(time(nullptr), id)) {
        audio_service_.LogAlarmAudioDiagnostics("touch");
        ESP_LOGI("AlarmDiag", "dismiss_confirmed id=%lu", static_cast<unsigned long>(id));
        audio_service_.ResetDecoder();
        audio_service_.SetRemoteAudioBlocked(false);
        Board::GetInstance().GetAudioCodec()->SetTemporaryOutputVolume(-1);
        Board::GetInstance().GetDisplay()->SetStatus(Lang::Strings::STANDBY);
        if (Board::GetInstance().IsResting() && !Board::GetInstance().SetRestAlarmVisible(false)) {
            ESP_LOGE("EchoEarRest", "Could not reblank after alarm");
        }
        // Publish completion last so startup cannot race cleanup after reboot.
        ringing_alarm_id_.store(0);
    } else ESP_LOGE("AlarmDiag", "dismiss_failed id=%lu", static_cast<unsigned long>(id));
    return true;
}

void Application::StartListening() {
    if (Board::GetInstance().IsResting() || rest_waking_) return;
    if (device_state_ == kDeviceStateActivating) {
        SetDeviceState(kDeviceStateIdle);
        return;
    } else if (device_state_ == kDeviceStateWifiConfiguring) {
        audio_service_.EnableAudioTesting(true);
        SetDeviceState(kDeviceStateAudioTesting);
        return;
    }

    if (!protocol_) {
        ESP_LOGE(TAG, "Protocol not initialized");
        return;
    }
    
    if (device_state_ == kDeviceStateIdle) {
        Schedule([this]() {
            if (device_state_ != kDeviceStateIdle) return;
            if (persistent_protocol_ && persistent_connecting_.load() && !persistent_link_enabled_) return;
            if (persistent_link_enabled_) {
                if (persistent_connecting_.load() || NetworkGenerationChanged(persistent_network_generation_.load(), persistent_network_handled_) || !protocol_->IsAudioChannelOpened()) {
                    persistent_pending_listen_ = true;
                    persistent_pending_mode_ = kListeningModeManualStop;
                    persistent_retry_.RetryNow();
                    SetDeviceState(kDeviceStateConnecting);
                    StartPersistentConnection();
                } else {
                    SetListeningMode(kListeningModeManualStop);
                }
                return;
            }
            if (!protocol_->IsAudioChannelOpened()) {
                SetDeviceState(kDeviceStateConnecting);
                if (!protocol_->OpenAudioChannel()) {
                    return;
                }
            }

            SetListeningMode(kListeningModeManualStop);
        });
    } else if (device_state_ == kDeviceStateSpeaking) {
        Schedule([this]() {
            AbortSpeaking(kAbortReasonNone);
            SetListeningMode(kListeningModeManualStop);
        });
    }
}

void Application::StopListening() {
    if (device_state_ == kDeviceStateAudioTesting) {
        audio_service_.EnableAudioTesting(false);
        SetDeviceState(kDeviceStateWifiConfiguring);
        return;
    }

    const std::array<int, 3> valid_states = {
        kDeviceStateListening,
        kDeviceStateSpeaking,
        kDeviceStateIdle,
    };
    // If not valid, do nothing
    if (std::find(valid_states.begin(), valid_states.end(), device_state_) == valid_states.end()) {
        return;
    }

    Schedule([this]() {
        if (device_state_ == kDeviceStateListening) {
            protocol_->SendStopListening();
            SetDeviceState(kDeviceStateIdle);
        }
    });
}

void Application::Start() {
    auto& board = Board::GetInstance();
    SetDeviceState(kDeviceStateStarting);

    /* Setup the display */
    auto display = board.GetDisplay();

    // Print board name/version info
    display->SetChatMessage("system", SystemInfo::GetUserAgent().c_str());

    /* Setup the audio service */
    auto codec = board.GetAudioCodec();
    audio_service_.Initialize(codec);
    audio_service_.Start();

    AudioServiceCallbacks callbacks;
    callbacks.on_send_queue_available = [this]() {
        xEventGroupSetBits(event_group_, MAIN_EVENT_SEND_AUDIO);
    };
    callbacks.on_wake_word_detected = [this](const std::string& wake_word) {
        xEventGroupSetBits(event_group_, MAIN_EVENT_WAKE_WORD_DETECTED);
    };
    callbacks.on_vad_change = [this](bool speaking) {
        xEventGroupSetBits(event_group_, MAIN_EVENT_VAD_CHANGE);
    };
    callbacks.on_touch_feedback_start = [this]() {
        if (touch_waiting_to_listen_.load()) touch_feedback_started_.store(true);
        Schedule([this]() {
            if (touch_waiting_to_listen_.load() && touch_ready_timer_handle_) {
                esp_timer_stop(touch_ready_timer_handle_);
                const int64_t left_us = std::max<int64_t>(1, 1000000 - (esp_timer_get_time() - touch_waiting_since_us_));
                esp_timer_start_once(touch_ready_timer_handle_, std::min<int64_t>(TouchFeedback::kMicMaskMs * 1000, left_us));
            }
        });
    };
    audio_service_.SetCallbacks(callbacks);

    // Start the main event loop task with priority 3
    xTaskCreate([](void* arg) {
        ((Application*)arg)->MainEventLoop();
        vTaskDelete(NULL);
    }, "main_event_loop", 2048 * 4, this, 3, &main_event_loop_task_handle_);

    /* Start the clock timer to update the status bar */
    esp_timer_start_periodic(clock_timer_handle_, 1000000);

    // Load persisted active alarms before waiting for Wi-Fi or OTA. All
    // access to the store remains serialized on the main event loop.
    std::atomic<bool> alarm_boot_ready{false};
    Schedule([this, &alarm_boot_ready]() {
        const bool alarm_loaded = alarm_store_.Load();
        ESP_LOGI("AlarmDiag", "init clock=%d storage=%d now=%lu", has_server_time_, alarm_loaded, static_cast<unsigned long>(static_cast<uint32_t>(time(nullptr))));
        if (!alarm_loaded) ESP_LOGE(TAG, "Alarm storage unavailable");
        nvs_handle_t alarm_cfg = 0;
        if (nvs_open("echo_alarms", NVS_READONLY, &alarm_cfg) == ESP_OK) {
            int32_t floor = 70;
            if (nvs_get_i32(alarm_cfg, "volume_floor", &floor) == ESP_OK && floor >= 0 && floor <= 100) alarm_volume_floor_ = floor;
            nvs_close(alarm_cfg);
        }
        PollAlarm();
        alarm_boot_ready.store(true);
    });

    // A restored alarm owns the display/audio until the user dismisses it.
    // Avoid startup sounds, OTA prompts and state changes racing that alarm.
    while (!alarm_boot_ready.load() || ringing_alarm_id_.load()) {
        vTaskDelay(pdMS_TO_TICKS(100));
    }

    // The legacy endpoint is erased last, so interruption remains retryable.
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    Schedule([this]() {
        legacy_cleanup_retry_ = RetryOldMqttCleanup(legacy_cleanup_started_);
        legacy_cleanup_next_us_ = esp_timer_get_time() + 30000000;
    });
#endif
    /* Wait for the network to be ready */
    board.StartNetwork();

    // Update the status bar immediately to show the network state
    display->UpdateStatusBar(true);

    // Check for new assets version
    CheckAssetsVersion();

    // Check for new firmware version or get the MQTT broker address
    Ota ota;
    CheckNewVersion(ota);

    // Initialize the protocol
    // Keep touch-triggered legacy opens disabled until capability negotiation
    // and Wi-Fi event registration have both been configured.
    SetDeviceState(kDeviceStateStarting);
    display->SetStatus(Lang::Strings::LOADING_PROTOCOL);

    // Add MCP common tools before initializing the protocol
    auto& mcp_server = McpServer::GetInstance();
    mcp_server.AddCommonTools();
    mcp_server.AddUserOnlyTools();

#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    // Never fall back to legacy MQTT, regardless of stale NVS or OTA response.
    protocol_ = std::make_unique<WebsocketProtocol>(&persistent_link_enabled_);
    persistent_protocol_ = static_cast<WebsocketProtocol*>(protocol_.get());
    persistent_refresh_ota_ = !ota.HasWebsocketConfig();
#else
    if (ota.HasMqttConfig()) {
        protocol_ = std::make_unique<MqttProtocol>();
    } else if (ota.HasWebsocketConfig()) {
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
        protocol_ = std::make_unique<WebsocketProtocol>(&persistent_link_enabled_);
        persistent_protocol_ = static_cast<WebsocketProtocol*>(protocol_.get());
#else
        protocol_ = std::make_unique<WebsocketProtocol>();
#endif
    } else {
        ESP_LOGW(TAG, "No protocol specified in the OTA config, using MQTT");
        protocol_ = std::make_unique<MqttProtocol>();
    }
#endif

    protocol_->OnConnected([this]() {
        DismissAlert();
    });

    protocol_->OnNetworkError([this](const std::string& message) {
        if (persistent_link_enabled_) {
            if (persistent_connecting_.load()) return;
            const auto epoch = persistent_epoch_.load();
            Schedule([this, epoch, message]() {
                if (ringing_alarm_id_.load()) return;
                if (!PersistentCallbackIsCurrent(persistent_link_enabled_, persistent_epoch_.load(), epoch) ||
                    persistent_connecting_.load()) return;
                SetDeviceState(kDeviceStateIdle);
                Alert(Lang::Strings::ERROR, message.c_str(), "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
            });
            return;
        }
        last_error_message_ = message;
        xEventGroupSetBits(event_group_, MAIN_EVENT_ERROR);
    });
    protocol_->OnIncomingAudio([this](std::unique_ptr<AudioStreamPacket> packet) {
        if (!ringing_alarm_id_.load() && device_state_ == kDeviceStateSpeaking) {
            audio_service_.PushPacketToDecodeQueue(std::move(packet), false, true);
        }
    });
    protocol_->OnAudioChannelOpened([this, codec, &board]() {
        board.SetPowerSaveMode(false);
        if (protocol_->server_sample_rate() != codec->output_sample_rate()) {
            ESP_LOGW(TAG, "Server sample rate %d does not match device output sample rate %d, resampling may cause distortion",
                protocol_->server_sample_rate(), codec->output_sample_rate());
        }
    });
    protocol_->OnAudioChannelClosed([this, &board]() {
        const auto epoch = persistent_epoch_.load();
        const bool persistent = persistent_link_enabled_.load();
        Schedule([this, &board, epoch, persistent]() {
            if (persistent && !PersistentCallbackIsCurrent(persistent_link_enabled_, persistent_epoch_.load(), epoch)) return;
            if (persistent_link_enabled_ &&
                (persistent_connecting_.load() || protocol_->IsAudioChannelOpened())) {
                // Callback belongs to an earlier socket, not a newer attempt.
                return;
            }
            if (board.IsResting()) rest_connection_lost_ = true;
            board.SetPowerSaveMode(true);
            auto display = Board::GetInstance().GetDisplay();
            display->SetChatMessage("system", "");
            if (persistent_link_enabled_ && ++persistent_listen_id_ == 0) ++persistent_listen_id_;
            SetDeviceState(kDeviceStateIdle);
            if (rest_waking_) PollRestWake();
        });
    });
    protocol_->OnIncomingJson([this, display](const cJSON* root, uint32_t image_epoch) {
        // Parse JSON data
        auto type = cJSON_GetObjectItem(root, "type");
        if (strcmp(type->valuestring, "hello") == 0) {
            const auto* transport = cJSON_GetObjectItem(root, "transport");
            const auto* sample = cJSON_GetObjectItem(root, "server_time");
            const auto* timestamp = cJSON_GetObjectItem(sample, "timestamp");
            const auto* offset = cJSON_GetObjectItem(sample, "timezone_offset");
            if (cJSON_IsString(transport) && strcmp(transport->valuestring, "websocket") == 0 &&
                cJSON_IsNumber(timestamp) && std::isfinite(timestamp->valuedouble) &&
                std::trunc(timestamp->valuedouble) == timestamp->valuedouble &&
                timestamp->valuedouble >= 1700000000000.0 && timestamp->valuedouble < 4102358400000.0 &&
                cJSON_IsNumber(offset) && offset->valuedouble == 300.0) {
                const int64_t server_ms = static_cast<int64_t>(timestamp->valuedouble) + 300 * 60 * 1000LL;
                const int64_t captured_us = esp_timer_get_time();
                Schedule([this, server_ms, captured_us, image_epoch]() {
                    if (!IsImageEpochCurrent(image_epoch)) return;
                    const int64_t corrected_us = server_ms * 1000 + (esp_timer_get_time() - captured_us);
                    timeval tv{};
                    tv.tv_sec = corrected_us / 1000000;
                    tv.tv_usec = corrected_us % 1000000;
                    if (settimeofday(&tv, nullptr) == 0) {
                        has_server_time_ = true;
                        ESP_LOGI(TAG, "Alarm clock synchronised from authenticated server");
                    } else {
                        ESP_LOGE(TAG, "Alarm clock synchronisation failed");
                    }
                });
            }
        } else if (strcmp(type->valuestring, "tts") == 0) {
            auto state = cJSON_GetObjectItem(root, "state");
            if (!cJSON_IsString(state)) return;
            auto id = cJSON_GetObjectItem(root, "listen_id");
            const bool has_id = cJSON_IsNumber(id) && id->valuedouble >= 1 && id->valuedouble <= UINT32_MAX &&
                static_cast<double>(static_cast<uint32_t>(id->valuedouble)) == id->valuedouble;
            const uint32_t listen_id = has_id ? static_cast<uint32_t>(id->valuedouble) : 0;
            if (strcmp(state->valuestring, "start") == 0) {
                Schedule([this, listen_id]() {
                    if (ringing_alarm_id_.load()) return;
                    if (!PersistentEventIsCurrent(persistent_link_enabled_, persistent_listen_id_, listen_id)) return;
                    if (Board::GetInstance().IsResting()) rest_tts_started_ = true;
                    aborted_ = false;
                    if (device_state_ == kDeviceStateIdle || device_state_ == kDeviceStateListening) {
                        SetDeviceState(kDeviceStateSpeaking);
                    }
                });
            } else if (strcmp(state->valuestring, "stop") == 0) {
                Schedule([this, listen_id]() {
                    if (ringing_alarm_id_.load()) return;
                    if (!PersistentEventIsCurrent(persistent_link_enabled_, persistent_listen_id_, listen_id)) return;
                    if (device_state_ == kDeviceStateSpeaking) {
                        if (Board::GetInstance().IsResting()) {
                            rest_tts_finished_ = true;
                            if (persistent_link_enabled_ && protocol_) {
                                protocol_->SendPauseListening(persistent_listen_id_);
                                if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
                            }
                            SetDeviceState(kDeviceStateIdle);
                        } else if (listening_mode_ == kListeningModeManualStop) {
                            SetDeviceState(kDeviceStateIdle);
                        } else {
                            SetDeviceState(kDeviceStateListening);
                        }
                    }
                });
            } else if (strcmp(state->valuestring, "sentence_start") == 0) {
                auto text = cJSON_GetObjectItem(root, "text");
                if (cJSON_IsString(text)) {
                    ESP_LOGI(TAG, "<< %s", text->valuestring);
                    Schedule([this, display, listen_id, message = std::string(text->valuestring)]() {
                        if (!PersistentEventIsCurrent(persistent_link_enabled_, persistent_listen_id_, listen_id)) return;
                        display->SetChatMessage("assistant", message.c_str());
                    });
                }
            }
        } else if (strcmp(type->valuestring, "standby") == 0 && persistent_link_enabled_) {
            auto id = cJSON_GetObjectItem(root, "listen_id");
            if (cJSON_IsNumber(id) && id->valuedouble >= 1 && id->valuedouble <= UINT32_MAX &&
                static_cast<double>(static_cast<uint32_t>(id->valuedouble)) == id->valuedouble) {
                const uint32_t listen_id = static_cast<uint32_t>(id->valuedouble);
                Schedule([this, listen_id]() {
                    if (PersistentStandbyIsCurrent(persistent_link_enabled_, persistent_listen_id_, listen_id,
                                                   device_state_ == kDeviceStateListening)) {
                        if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
                        SetDeviceState(kDeviceStateIdle);
                        audio_service_.ResetDecoder();
                    }
                });
            }
        } else if (strcmp(type->valuestring, "stt") == 0) {
            auto text = cJSON_GetObjectItem(root, "text");
            if (cJSON_IsString(text)) {
                ESP_LOGI(TAG, ">> %s", text->valuestring);
                Schedule([this, display, message = std::string(text->valuestring)]() {
                    display->SetChatMessage("user", message.c_str());
                });
            }
        } else if (strcmp(type->valuestring, "llm") == 0) {
            auto emotion = cJSON_GetObjectItem(root, "emotion");
            if (cJSON_IsString(emotion)) {
                Schedule([this, display, emotion_str = std::string(emotion->valuestring)]() {
                    display->SetEmotion(emotion_str.c_str());
                });
            }
        } else if (strcmp(type->valuestring, "image_preview") == 0) {
            display->HandleImagePreview(root, image_epoch);
        } else if (strcmp(type->valuestring, "mcp") == 0) {
            auto payload = cJSON_GetObjectItem(root, "payload");
            if (cJSON_IsObject(payload)) {
                McpServer::GetInstance().ParseMessage(payload, image_epoch);
            }
        } else if (strcmp(type->valuestring, "system") == 0) {
            auto command = cJSON_GetObjectItem(root, "command");
            if (cJSON_IsString(command)) {
                ESP_LOGI(TAG, "System command: %s", command->valuestring);
                if (strcmp(command->valuestring, "reboot") == 0) {
                    // Do a reboot if user requests a OTA update
                    Schedule([this]() {
                        Reboot();
                    });
                } else {
                    ESP_LOGW(TAG, "Unknown system command: %s", command->valuestring);
                }
            }
        } else if (strcmp(type->valuestring, "alert") == 0) {
            auto status = cJSON_GetObjectItem(root, "status");
            auto message = cJSON_GetObjectItem(root, "message");
            auto emotion = cJSON_GetObjectItem(root, "emotion");
            if (cJSON_IsString(status) && cJSON_IsString(message) && cJSON_IsString(emotion) && !ringing_alarm_id_.load()) {
                Alert(status->valuestring, message->valuestring, emotion->valuestring, Lang::Sounds::OGG_VIBRATION);
            } else {
                ESP_LOGW(TAG, "Alert command requires status, message and emotion");
            }
#if CONFIG_RECEIVE_CUSTOM_MESSAGE
        } else if (strcmp(type->valuestring, "custom") == 0) {
            auto payload = cJSON_GetObjectItem(root, "payload");
            ESP_LOGI(TAG, "Received custom message: %s", cJSON_PrintUnformatted(root));
            if (cJSON_IsObject(payload)) {
                Schedule([this, display, payload_str = std::string(cJSON_PrintUnformatted(payload))]() {
                    display->SetChatMessage("system", payload_str.c_str());
                });
            } else {
                ESP_LOGW(TAG, "Invalid custom message format: missing payload");
            }
#endif
        } else {
            ESP_LOGW(TAG, "Unknown message type: %s", type->valuestring);
        }
    });
    bool protocol_started = protocol_->Start();

    if (persistent_protocol_) {
        // Events are monotonic even if Wi-Fi disconnects and reconnects
        // between two main-loop ticks. No socket exists before this baseline.
        const auto on_wifi_event = [](void* context, esp_event_base_t base, int32_t id, void* data) {
            auto* app = static_cast<Application*>(context);
            const bool disconnected = base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED;
            const bool ip_changed = base == IP_EVENT && id == IP_EVENT_STA_GOT_IP && data &&
                static_cast<ip_event_got_ip_t*>(data)->ip_changed;
            if (disconnected || ip_changed) {
                app->persistent_network_generation_.fetch_add(1);
                xEventGroupSetBits(app->event_group_, MAIN_EVENT_WIFI_CHANGED);
            }
        };
        esp_err_t wifi_event_result = esp_event_handler_register(WIFI_EVENT, WIFI_EVENT_STA_DISCONNECTED, on_wifi_event, this);
        esp_err_t ip_event_result = esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_wifi_event, this);
        if (wifi_event_result != ESP_OK || ip_event_result != ESP_OK) {
            ESP_LOGE(TAG, "Could not register network lifecycle events");
        } else {
            persistent_network_events_ready_ = true;
        }
        persistent_network_handled_ = persistent_network_generation_.load();
    }

    if (persistent_protocol_ != nullptr && persistent_network_events_ready_) {
        persistent_link_enabled_.store(true);
    }
    SystemInfo::PrintHeapStats();
    SetDeviceState(kDeviceStateIdle);

    // The OTA clock is trusted only when the OTA response supplied it.
    const bool ota_clock_valid = ota.HasServerTime();
    Schedule([this, ota_clock_valid]() {
        if (ota_clock_valid) has_server_time_ = true;
    });
    if (persistent_link_enabled_.load()) Schedule([this]() { StartPersistentConnection(); });
    if (protocol_started) {
        std::string message = std::string(Lang::Strings::VERSION) + ota.GetCurrentVersion();
        display->ShowNotification(message.c_str());
        display->SetChatMessage("system", "");
        // Play the success sound to indicate the device is ready
        audio_service_.PlaySound(Lang::Sounds::OGG_SUCCESS);
    }
}

static std::string AlarmError(echoalarm::Error error) {
    const char* value = "invalid";
    switch (error) {
      case echoalarm::Error::InvalidClock: value = "clock_unsynchronised"; break;
      case echoalarm::Error::InvalidArgument: value = "invalid_argument_or_past"; break;
      case echoalarm::Error::Full: value = "alarm_list_full"; break;
      case echoalarm::Error::NotFound: value = "alarm_not_found"; break;
      case echoalarm::Error::StorageFailure: value = "storage_unavailable"; break;
      default: break;
    }
    return std::string("{\"ok\":false,\"error\":\"") + value + "\"}";
}

std::string Application::SetAlarm(int hour, int minute, int day, bool daily) {
    const auto now = time(nullptr);
    ESP_LOGI("AlarmDiag", "set_absolute now=%lu clock=%d day=%d daily=%d", static_cast<unsigned long>(static_cast<uint32_t>(now)), has_server_time_, day, daily);
    if (!has_server_time_) { ESP_LOGE("AlarmDiag", "set_absolute clock_invalid"); return AlarmError(echoalarm::Error::InvalidClock); }
    auto result = alarm_store_.Create(now, hour, minute, day, daily);
    if (result.error != echoalarm::Error::Ok) { ESP_LOGE("AlarmDiag", "set_absolute failed code=%d", static_cast<int>(result.error)); return AlarmError(result.error); }
    for (const auto& a : result.state.alarms) if (a.id == result.id) {
        ESP_LOGI("AlarmDiag", "saved id=%lu now=%lu due=%lu", static_cast<unsigned long>(a.id), static_cast<unsigned long>(static_cast<uint32_t>(now)), static_cast<unsigned long>(static_cast<uint32_t>(a.next)));
        return "{\"ok\":true,\"id\":" + std::to_string(a.id) + ",\"local_epoch\":" + std::to_string(a.next) +
               ",\"daily\":" + (daily ? "true" : "false") + "}";
    }
    return AlarmError(echoalarm::Error::StorageFailure);
}

std::string Application::SetAlarmAfter(int minutes) {
    ESP_LOGI("AlarmDiag", "set_after minutes=%d clock=%d", minutes, has_server_time_);
    if (!has_server_time_) { ESP_LOGE("AlarmDiag", "set_after clock_invalid"); return AlarmError(echoalarm::Error::InvalidClock); }
    timeval now{};
    if (gettimeofday(&now, nullptr) != 0) { ESP_LOGE("AlarmDiag", "set_after gettimeofday_failed"); return AlarmError(echoalarm::Error::InvalidClock); }
    auto result = alarm_store_.CreateAfter(now.tv_sec, static_cast<int>(now.tv_usec), minutes);
    if (result.error != echoalarm::Error::Ok) { ESP_LOGE("AlarmDiag", "set_after failed code=%d", static_cast<int>(result.error)); return AlarmError(result.error); }
    for (const auto& a : result.state.alarms) if (a.id == result.id) {
        ESP_LOGI("AlarmDiag", "saved id=%lu now=%lu due=%lu", static_cast<unsigned long>(a.id), static_cast<unsigned long>(static_cast<uint32_t>(now.tv_sec)), static_cast<unsigned long>(static_cast<uint32_t>(a.next)));
        return "{\"ok\":true,\"id\":" + std::to_string(a.id) + ",\"local_epoch\":" + std::to_string(a.next) +
               ",\"daily\":false}";
    }
    return AlarmError(echoalarm::Error::StorageFailure);
}

std::string Application::DeleteAlarm(uint32_t id) {
    auto result = alarm_store_.Remove(id);
    ESP_LOGI("AlarmDiag", "delete id=%lu code=%d", static_cast<unsigned long>(id), static_cast<int>(result.error));
    if (result.error != echoalarm::Error::Ok) return AlarmError(result.error);
    if (ringing_alarm_id_.load() == id) {
        ringing_alarm_id_.store(0);
        audio_service_.ResetDecoder();
        audio_service_.SetRemoteAudioBlocked(false);
        Board::GetInstance().GetAudioCodec()->SetTemporaryOutputVolume(-1);
        Board::GetInstance().GetDisplay()->SetStatus(Lang::Strings::STANDBY);
        if (Board::GetInstance().IsResting()) Board::GetInstance().SetRestAlarmVisible(false);
    }
    return "{\"ok\":true}";
}

std::string Application::ListAlarms() const { ESP_LOGI("AlarmDiag", "list requested"); return alarm_store_.Describe(); }

bool Application::SetAlarmVolumeFloor(int floor) {
    if (floor < 0 || floor > 100) return false;
    nvs_handle_t handle = 0;
    if (nvs_open("echo_alarms", NVS_READWRITE, &handle) != ESP_OK) return false;
    auto result = nvs_set_i32(handle, "volume_floor", floor);
    if (result == ESP_OK) result = nvs_commit(handle);
    nvs_close(handle);
    if (result != ESP_OK) return false;
    alarm_volume_floor_ = floor;
    if (ringing_alarm_id_.load()) {
        auto* codec = Board::GetInstance().GetAudioCodec();
        codec->SetTemporaryOutputVolume(floor);
    }
    return true;
}

void Application::PollAlarm() {
    const int64_t diag_us = esp_timer_get_time();
    if (!last_alarm_diag_us_ || diag_us - last_alarm_diag_us_ >= 30000000) {
        last_alarm_diag_us_ = diag_us;
        ESP_LOGI("AlarmDiag", "tick now=%lu clock=%d ringing_id=%lu", static_cast<unsigned long>(static_cast<uint32_t>(time(nullptr))), has_server_time_, static_cast<unsigned long>(ringing_alarm_id_.load()));
    }
    // An already-ringing alarm is persisted and must resume even before the
    // clock has been synchronized after a reboot. Pending alarms must never
    // be evaluated using the untrusted boot clock.
    auto state = alarm_store_.Tick(has_server_time_ ? time(nullptr) : 0);
    auto* codec = Board::GetInstance().GetAudioCodec();
    if (!state.ring) {
        if (ringing_alarm_id_.exchange(0)) {
            audio_service_.LogAlarmAudioDiagnostics("stop");
            ESP_LOGI("AlarmDiag", "ring_stopped now=%lu", static_cast<unsigned long>(static_cast<uint32_t>(time(nullptr))));
            audio_service_.ResetDecoder();
            audio_service_.SetRemoteAudioBlocked(false);
            codec->SetTemporaryOutputVolume(-1);
            Board::GetInstance().GetDisplay()->SetStatus(Lang::Strings::STANDBY);
            if (Board::GetInstance().IsResting()) Board::GetInstance().SetRestAlarmVisible(false);
        }
        return;
    }
    if (ringing_alarm_id_.exchange(state.id) != state.id) {
        if (Board::GetInstance().IsResting() && !rest_transport_retired_) {
            // Alarm must not wait for a stalled close worker. Prevent new
            // traffic immediately, then retire the socket asynchronously.
            if (persistent_protocol_) persistent_protocol_->SuspendTraffic();
            rest_close_pending_ = true;
            if (StartPersistentClose()) rest_close_pending_ = false;
        }
        if (Board::GetInstance().IsResting() && !Board::GetInstance().SetRestAlarmVisible(true)) {
            ESP_LOGE("EchoEarRest", "Could not illuminate alarm");
        }
        audio_service_.ResetAlarmAudioDiagnostics();
        ESP_LOGI("AlarmDiag", "ring_start id=%lu now=%lu floor=%d normal=%d", static_cast<unsigned long>(state.id), static_cast<unsigned long>(static_cast<uint32_t>(time(nullptr))), alarm_volume_floor_, codec->output_volume());
        audio_service_.SetRemoteAudioBlocked(true);
        if (device_state_ == kDeviceStateSpeaking) AbortSpeaking(kAbortReasonNone);
        if (device_state_ == kDeviceStateListening && protocol_) protocol_->SendPauseListening(persistent_listen_id_);
        if (persistent_link_enabled_ && ++persistent_listen_id_ == 0) ++persistent_listen_id_;
        audio_service_.ResetDecoder();
        SetDeviceState(kDeviceStateIdle);
        const bool volume_ok = codec->SetTemporaryOutputVolume(alarm_volume_floor_);
        ESP_LOGI("AlarmDiag", "volume_override ok=%d output_enabled=%d", volume_ok, codec->output_enabled());
        Board::GetInstance().GetDisplay()->SetStatus("Alarm");
        last_alarm_sound_us_ = 0;
    }
    const int64_t now_us = esp_timer_get_time();
    // The 1.92-second chime completes before the next playback is queued.
    if (!last_alarm_sound_us_ || now_us - last_alarm_sound_us_ >= 3000000) {
        last_alarm_sound_us_ = now_us;
        audio_service_.PlaySound(Lang::Sounds::OGG_ALARM_BEEP, true);
        audio_service_.LogAlarmAudioDiagnostics("queued");
    }
}

bool Application::StartRestWakePrompt(bool ready) {
    if (rest_wake_prompt_feeding_.load()) return false;
    struct WakeJob { Application* app; uint32_t generation; bool ready; };
    auto* job = new (std::nothrow) WakeJob{this, audio_service_.PlaybackGeneration(), ready};
    if (!job) return false;
    rest_wake_audio_generation_ = job->generation;
    rest_wake_socket_generation_ = persistent_protocol_ ? persistent_protocol_->ImageEpoch() : 0;
    rest_wake_network_generation_ = persistent_network_generation_.load();
    rest_wake_drained_since_us_ = 0;
    audio_service_.ResetWakePromptStats();
    rest_wake_prompt_feeding_.store(true);
    const auto created = xTaskCreate([](void* arg) {
        auto* job = static_cast<WakeJob*>(arg);
        auto* app = job->app;
        const auto generation = job->generation;
        const bool ready = job->ready;
        delete job;
        app->audio_service_.PlaySoundGuarded(ready ? Lang::Sounds::OGG_REST_AWAKE : GetEchoEarRestWakeSound(), generation);
        app->rest_wake_prompt_feeding_.store(false);
        xEventGroupSetBits(app->event_group_, MAIN_EVENT_SCHEDULE);
        vTaskDelete(nullptr);
    }, "rest_wake_sound", 4096, job, 3, nullptr);
    if (created != pdPASS) {
        delete job;
        rest_wake_prompt_feeding_.store(false);
        return false;
    }
    return true;
}

bool Application::SetRestMode(bool enabled) {
    auto& board = Board::GetInstance();
    if (enabled == board.IsResting()) return true;
    if (IsAlarmRinging() || !persistent_link_enabled_ || !persistent_protocol_ || !protocol_) return false;
    if (enabled) {
        if (rest_waking_ || persistent_connecting_.load() || !protocol_->IsAudioChannelOpened() ||
            (device_state_ != kDeviceStateIdle && device_state_ != kDeviceStateListening &&
             device_state_ != kDeviceStateSpeaking)) return false;
        if (!board.SetRestMode(true)) return false;
        touch_waiting_to_listen_.store(false);
        if (touch_ready_timer_handle_) esp_timer_stop(touch_ready_timer_handle_);
        audio_service_.CancelPendingTouchFeedback();
        rest_close_pending_ = true;
        rest_transport_retired_ = false;
        rest_connection_lost_ = false;
        rest_close_timeout_reported_ = false;
        rest_tts_started_ = device_state_ == kDeviceStateSpeaking;
        // Even an idle device must allow the bridge to start its spoken
        // acknowledgement after receiving this tool result.
        rest_tts_finished_ = false;
        rest_started_us_ = esp_timer_get_time();
        rest_drained_since_us_ = 0;
        persistent_pending_listen_ = false;
        persistent_pending_wake_ = false;
        audio_service_.EnableVoiceProcessing(false);
        audio_service_.EnableWakeWordDetection(false);
        return true;
    }

    if (!board.SetRestMode(false)) return false;
    rest_close_pending_ = false;
    rest_waking_ = true;
    rest_wake_phase_ = echowake::Phase::First;
    rest_wake_drained_since_us_ = 0;
    if (device_state_ == kDeviceStateSpeaking) AbortSpeaking(kAbortReasonNone);
    if (device_state_ == kDeviceStateListening && !persistent_connecting_.load() && protocol_->IsAudioChannelOpened()) {
        protocol_->SendPauseListening(persistent_listen_id_);
    }
    if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
    audio_service_.EnableVoiceProcessing(false);
    audio_service_.EnableWakeWordDetection(false);
    audio_service_.ResetDecoder();
    if (!StartRestWakePrompt(false)) {
        rest_waking_ = false;
        if (board.SetRestMode(true)) {
            rest_close_pending_ = !rest_transport_retired_;
        } else {
            ESP_LOGE("EchoEarRest", "Could not restore rest after wake failure");
        }
        return false;
    }
    SetDeviceState(kDeviceStateConnecting);
    persistent_retry_.RetryNow();
    StartPersistentConnection();
    return true;
}

void Application::PollRestConnection() {
    if (!Board::GetInstance().IsResting() || !rest_close_pending_ || rest_transport_retired_ || !protocol_) return;
    const int64_t now = esp_timer_get_time();
    const bool drained = audio_service_.IsPlaybackDrained();
    if (drained) {
        if (!rest_drained_since_us_) rest_drained_since_us_ = now;
    } else rest_drained_since_us_ = 0;
    const bool stable_drain = rest_drained_since_us_ && now - rest_drained_since_us_ >= echorest::kDrainSettleUs;
    const echorest::CloseInput input{rest_close_pending_, rest_transport_retired_, IsAlarmRinging(),
        rest_connection_lost_, rest_tts_started_, rest_tts_finished_, stable_drain, now - rest_started_us_};
    if (echorest::ShouldClose(input)) {
        if (StartPersistentClose()) rest_close_pending_ = false;
    }
}

void Application::PollRestWake() {
    if (!rest_waking_) return;
    const int64_t now = esp_timer_get_time();
    if (!rest_wake_prompt_feeding_.load() && audio_service_.IsPlaybackDrained()) {
        if (!rest_wake_drained_since_us_) rest_wake_drained_since_us_ = now;
    } else rest_wake_drained_since_us_ = 0;
    const bool sound_done = rest_wake_drained_since_us_ && now - rest_wake_drained_since_us_ >= echorest::kDrainSettleUs;
    const bool transport_eligible = WifiStation::GetInstance().IsConnected() && persistent_link_enabled_ && !persistent_connecting_.load() &&
        !NetworkGenerationChanged(persistent_network_generation_.load(), persistent_network_handled_) && persistent_protocol_ &&
        persistent_protocol_->SupportsPersistentIdle() && !persistent_protocol_->TrafficSuspended() && protocol_;
    const auto channel_state = transport_eligible ? persistent_protocol_->TryGetAudioChannelState() : WebsocketProtocol::AudioChannelState::Closed;
    const bool connected = channel_state == WebsocketProtocol::AudioChannelState::Open;
    const bool busy = channel_state == WebsocketProtocol::AudioChannelState::Busy;
    const bool same_connection = persistent_protocol_ &&
        persistent_protocol_->ImageEpoch() == rest_wake_socket_generation_ &&
        persistent_network_generation_.load() == rest_wake_network_generation_;
    const echowake::Input input{rest_wake_phase_, IsAlarmRinging(), connected, busy,
        rest_wake_audio_generation_ == audio_service_.PlaybackGeneration(), same_connection,
        rest_wake_prompt_feeding_.load(), sound_done, audio_service_.WakePromptSucceeded()};
    switch (echowake::Decide(input)) {
    case echowake::Action::RestartFirst:
        if (!StartRestWakePrompt(false)) ESP_LOGE("EchoEarRest", "Could not replay wake prompt");
        break;
    case echowake::Action::FirstFinished:
        rest_wake_phase_ = echowake::Phase::AwaitReady;
        break;
    case echowake::Action::StartReady:
        if (StartRestWakePrompt(true)) rest_wake_phase_ = echowake::Phase::Ready;
        else ESP_LOGE("EchoEarRest", "Could not start ready prompt");
        break;
    case echowake::Action::CancelReady:
        // This invalidates the guarded producer and drops queued audio. One
        // block already submitted to I2S may still finish.
        if (input.same_audio_generation) audio_service_.ResetDecoder();
        rest_wake_drained_since_us_ = 0;
        rest_wake_phase_ = echowake::Phase::AwaitReady;
        break;
    case echowake::Action::Done:
        rest_waking_ = false;
        SetDeviceState(kDeviceStateIdle);
        break;
    case echowake::Action::Wait:
        break;
    }
}

// Add a async task to MainLoop
void Application::Schedule(std::function<void()> callback) {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        main_tasks_.push_back(std::move(callback));
    }
    xEventGroupSetBits(event_group_, MAIN_EVENT_SCHEDULE);
}

void Application::StartPersistentConnection() {
    // Called only from the main event loop. Never run a TLS handshake there:
    // even a failed handshake can take several seconds.
    if (Board::GetInstance().IsResting() || !persistent_link_enabled_ || !protocol_ || persistent_connecting_.load() ||
        !WifiStation::GetInstance().IsConnected() || !persistent_retry_.Due(esp_timer_get_time())) {
        return;
    }
    if (NetworkGenerationChanged(persistent_network_generation_.load(), persistent_network_handled_)) {
        ProcessPersistentNetworkChange();
        return;
    }
    if (rest_waking_ && persistent_protocol_ && persistent_protocol_->TrafficSuspended()) {
        StartPersistentClose();
        return;
    }
    if (persistent_protocol_) {
        // Busy is unknown, not disconnected; retry a later tick instead of
        // replacing a healthy socket while its mutex is held.
        if (!AudioChannelNeedsReconnect(persistent_protocol_->TryGetAudioChannelState())) return;
    } else if (protocol_->IsAudioChannelOpened()) return;
    // Invalidate messages queued by an earlier socket before starting a new one.
    if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
    persistent_connecting_.store(true);
    persistent_worker_active_.store(true);
    struct Attempt { Application* app; uint32_t epoch; uint32_t network_generation; bool refresh_ota; };
    auto* attempt = new Attempt{this, ++persistent_epoch_, persistent_network_generation_.load(), persistent_refresh_ota_};
    const BaseType_t created = xTaskCreate([](void* arg) {
        auto* attempt = static_cast<Attempt*>(arg);
        auto* app = attempt->app;
        const auto epoch = attempt->epoch;
        const auto network_generation = attempt->network_generation;
        const bool refresh_ota = attempt->refresh_ota;
        delete attempt;
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
        Settings cached("websocket", false);
        bool can_open = true;
        if (endpoint_policy::NeedOtaRefresh(refresh_ota, GetDeploymentOtaUrl(), cached.GetString("url"))) {
            Ota recovery;
            const bool refreshed = recovery.CheckVersion(false) == ESP_OK && recovery.HasWebsocketConfig();
            can_open = refreshed;
            if (refreshed && recovery.HasServerTimeSample()) {
                const int64_t server_ms = recovery.ServerTimeSampleMs();
                const int64_t captured_us = esp_timer_get_time();
                app->Schedule([app, epoch, network_generation, server_ms, captured_us]() {
                    if (!RecoveryClockSampleIsCurrent(app->persistent_link_enabled_, app->persistent_epoch_.load(), epoch,
                            network_generation, app->persistent_network_generation_.load(), app->persistent_network_handled_)) return;
                    const int64_t corrected_us = server_ms * 1000 + (esp_timer_get_time() - captured_us);
                    timeval tv{};
                    tv.tv_sec = corrected_us / 1000000;
                    tv.tv_usec = corrected_us % 1000000;
                    if (settimeofday(&tv, nullptr) == 0) app->has_server_time_ = true;
                });
            }
        }
#else
        (void)refresh_ota;
        const bool can_open = true;
#endif
        const bool opened = can_open && app->protocol_->OpenAudioChannel();
        app->persistent_worker_active_.store(false);
        app->Schedule([app, opened, epoch, network_generation]() {
            app->persistent_connecting_.store(false);
            if (!PersistentCallbackIsCurrent(app->persistent_link_enabled_, app->persistent_epoch_.load(), epoch)) return;
            if (!NetworkAttemptIsCurrent(network_generation, app->persistent_network_generation_.load(),
                                         app->persistent_network_handled_)) {
                app->ProcessPersistentNetworkChange();
                return;
            }
            if (!opened) {
                app->persistent_refresh_ota_ = true;
                app->persistent_pending_mcp_.clear();
                app->persistent_retry_.Failed(esp_timer_get_time(), esp_random());
                const bool requested = app->persistent_pending_listen_ || app->persistent_pending_wake_;
                app->persistent_pending_listen_ = false;
                app->persistent_pending_wake_ = false;
                if (requested) {
                    app->SetDeviceState(kDeviceStateIdle);
                    app->Alert(Lang::Strings::ERROR, Lang::Strings::SERVER_NOT_CONNECTED, "circle_xmark");
                }
                return;
            }
            app->persistent_retry_.Succeeded();
            app->persistent_refresh_ota_ = false;
            if (!app->persistent_protocol_->SupportsPersistentIdle()) {
                if (app->rest_waking_) {
                    ESP_LOGE("EchoEarRest", "Persistent reconnect unsupported");
                    app->StartPersistentClose();
                    return;
                }
                // Negotiate explicitly. A stock server keeps the stock flow.
                app->persistent_link_enabled_ = false;
                app->persistent_pending_mcp_.clear();
                if (!app->persistent_pending_listen_ && !app->persistent_pending_wake_) {
                    // Keep the negotiated legacy channel for the next touch;
                    // closing it here would block the main event loop.
                    return;
                }
            }
            auto mcp = std::move(app->persistent_pending_mcp_);
            app->persistent_pending_mcp_.clear();
            for (const auto& message : mcp) {
                app->protocol_->SendMcpMessage(message);
            }
            const bool wake = app->persistent_pending_wake_;
            const bool listen = app->persistent_pending_listen_;
            app->persistent_pending_wake_ = false;
            app->persistent_pending_listen_ = false;
            if (wake || listen) {
                // Enter idle first: the ordinary wake/touch path checks it.
                app->SetDeviceState(kDeviceStateIdle);
                if (wake) app->OnWakeWordDetected();
                else app->SetListeningMode(app->persistent_pending_mode_);
            }
        });
        vTaskDelete(nullptr);
    }, "persistent_link", 8192, attempt, 3, nullptr);
    if (created != pdPASS) {
        delete attempt;
        persistent_worker_active_.store(false);
        persistent_connecting_.store(false);
        persistent_retry_.Failed(esp_timer_get_time(), esp_random());
    }
}

void Application::ProcessPersistentNetworkChange() {
    if (Board::GetInstance().IsResting()) return;
    if (!persistent_link_enabled_ || persistent_connecting_.load()) return;
    const uint32_t observed = persistent_network_generation_.load();
    if (!NetworkGenerationChanged(observed, persistent_network_handled_)) return;
    // Close even when the old socket already reports disconnected: its TLS
    // object must be retired before opening a socket on the new interface.
    if (StartPersistentClose()) persistent_network_handled_ = observed;
}

bool Application::StartPersistentClose() {
    // Called on the event loop, only after the connection worker is idle.
    if (!protocol_ || persistent_connecting_.load() || !async_close_.Begin(esp_timer_get_time())) return false;
    persistent_connecting_.store(true);
    persistent_worker_active_.store(true);
    ++persistent_epoch_;
    if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
    if (device_state_ == kDeviceStateListening || device_state_ == kDeviceStateSpeaking) {
        SetDeviceState(kDeviceStateIdle);
    }
    const BaseType_t created = xTaskCreate([](void* arg) {
        auto* app = static_cast<Application*>(arg);
        app->protocol_->CloseAudioChannel();
        // No protocol access is permitted after publishing completion.
        app->persistent_worker_active_.store(false);
        app->async_close_.Complete();
        xEventGroupSetBits(app->event_group_, MAIN_EVENT_ASYNC_CLOSE_DONE);
        vTaskDelete(nullptr);
    }, "persistent_close", 4096, this, 3, nullptr);
    if (created != pdPASS) {
        persistent_worker_active_.store(false);
        persistent_connecting_.store(false);
        async_close_.CancelUnstarted();
        ESP_LOGE(TAG, "Could not create audio channel close worker");
        return false;
    }
    return true;
}

void Application::PollPersistentClose() {
    switch (async_close_.Poll(esp_timer_get_time())) {
    case AsyncClosePolicy::Result::Complete:
        persistent_connecting_.store(false);
        rest_close_timeout_reported_ = false;
        if (Board::GetInstance().IsResting()) { rest_transport_retired_ = true; rest_close_pending_ = false; }
        persistent_retry_.RetryNow();
        if (upgrade_pending_) {
            auto url = std::move(pending_upgrade_url_);
            upgrade_pending_ = false;
            upgrade_close_completed_ = true;
            Ota ota;
            if (!UpgradeFirmware(ota, url)) ESP_LOGE(TAG, "Firmware upgrade failed");
        }
        break;
    case AsyncClosePolicy::Result::TimedOut:
        if (Board::GetInstance().IsResting() ||
            (rest_waking_ && (IsAlarmRinging() || rest_wake_prompt_feeding_.load() ||
                !rest_wake_drained_since_us_ ||
                esp_timer_get_time() - rest_wake_drained_since_us_ < echorest::kDrainSettleUs))) {
            // Never interrupt a local rest alarm to restart a stuck transport.
            // The worker keeps ownership, and reconnect remains suppressed.
            if (!rest_close_timeout_reported_) {
                ESP_LOGE("EchoEarRest", "Close worker timed out; rest remains offline-pending");
                rest_close_timeout_reported_ = true;
            }
            break;
        }
        // A blocked worker retains its live objects until the whole device
        // restarts; never free those objects from this event loop.
        ESP_LOGE(TAG, "Audio channel close timed out; restarting");
        esp_restart();
        break;
    default:
        break;
    }
}

// The Main Event Loop controls the chat state and websocket connection
// If other tasks need to access the websocket or chat state,
// they should use Schedule to call this function
void Application::MainEventLoop() {
    while (true) {
        auto bits = xEventGroupWaitBits(event_group_, MAIN_EVENT_SCHEDULE |
            MAIN_EVENT_SEND_AUDIO |
            MAIN_EVENT_WAKE_WORD_DETECTED |
            MAIN_EVENT_VAD_CHANGE |
            MAIN_EVENT_CLOCK_TICK |
            MAIN_EVENT_ASYNC_CLOSE_DONE |
            MAIN_EVENT_WIFI_CHANGED |
            MAIN_EVENT_ERROR, pdTRUE, pdFALSE, portMAX_DELAY);

        if (bits & (MAIN_EVENT_ASYNC_CLOSE_DONE | MAIN_EVENT_CLOCK_TICK)) {
            PollPersistentClose();
        }

        if (bits & (MAIN_EVENT_WIFI_CHANGED | MAIN_EVENT_ASYNC_CLOSE_DONE | MAIN_EVENT_CLOCK_TICK)) {
            ProcessPersistentNetworkChange();
        }
        if (bits & (MAIN_EVENT_WIFI_CHANGED | MAIN_EVENT_ASYNC_CLOSE_DONE)) {
            if (rest_waking_) PollRestWake();
        }

        if (bits & MAIN_EVENT_ERROR) {
            SetDeviceState(kDeviceStateIdle);
            Alert(Lang::Strings::ERROR, last_error_message_.c_str(), "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
        }

        if (bits & MAIN_EVENT_SEND_AUDIO) {
            while (auto packet = audio_service_.PopPacketFromSendQueue()) {
                if (Board::GetInstance().IsResting() || rest_waking_) continue;
                if (persistent_link_enabled_ &&
                    (persistent_connecting_.load() ||
                     (device_state_ != kDeviceStateListening && device_state_ != kDeviceStateSpeaking))) {
                    // Drain stale capture after standby; idle must not send audio.
                    continue;
                }
                if (protocol_ && !protocol_->SendAudio(std::move(packet))) {
                    break;
                }
            }
        }

        if (bits & MAIN_EVENT_WAKE_WORD_DETECTED) {
            OnWakeWordDetected();
        }

        if (bits & MAIN_EVENT_VAD_CHANGE) {
            if (device_state_ == kDeviceStateListening) {
                auto led = Board::GetInstance().GetLed();
                led->OnStateChanged();
            }
        }

        if (bits & MAIN_EVENT_SCHEDULE) {
            std::unique_lock<std::mutex> lock(mutex_);
            auto tasks = std::move(main_tasks_);
            lock.unlock();
            for (auto& task : tasks) {
                task();
            }
        }

        if (bits & MAIN_EVENT_CLOCK_TICK) {
            clock_ticks_++;
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
            if (legacy_cleanup_retry_ && esp_timer_get_time() >= legacy_cleanup_next_us_) {
                legacy_cleanup_retry_ = RetryOldMqttCleanup(legacy_cleanup_started_);
                legacy_cleanup_next_us_ = esp_timer_get_time() + 30000000;
            }
#endif
            PollAlarm();
            PollRestConnection();
            PollRestWake();
            auto display = Board::GetInstance().GetDisplay();
            display->UpdateStatusBar();

            if (persistent_link_enabled_ && !Board::GetInstance().IsResting()) {
                const bool wifi = WifiStation::GetInstance().IsConnected();
                if (wifi && !persistent_wifi_was_connected_) persistent_retry_.RetryNow();
                persistent_wifi_was_connected_ = wifi;
                if (!persistent_connecting_.load()) {
                    const auto channel = persistent_protocol_ ? persistent_protocol_->TryGetAudioChannelState() :
                        (protocol_->IsAudioChannelOpened() ? WebsocketProtocol::AudioChannelState::Open : WebsocketProtocol::AudioChannelState::Closed);
                    if (!wifi && channel == WebsocketProtocol::AudioChannelState::Open) StartPersistentClose();
                    if (wifi && AudioChannelNeedsReconnect(channel) &&
                        device_state_ != kDeviceStateUpgrading && device_state_ != kDeviceStateWifiConfiguring) {
                        if (device_state_ == kDeviceStateListening || device_state_ == kDeviceStateSpeaking) {
                            SetDeviceState(kDeviceStateIdle);
                        }
                        StartPersistentConnection();
                    }
                }
            }
        
            // Print the debug info every 10 seconds
            if (clock_ticks_ % 10 == 0) {
                // SystemInfo::PrintTaskCpuUsage(pdMS_TO_TICKS(1000));
                // SystemInfo::PrintTaskList();
                SystemInfo::PrintHeapStats();
            }
        }
    }
}

void Application::OnWakeWordDetected() {
    if (Board::GetInstance().IsResting() || rest_waking_) return;
    if (ringing_alarm_id_.load()) return;
    if (!protocol_) {
        return;
    }
    if (persistent_protocol_ && persistent_connecting_.load() && !persistent_link_enabled_) return;

    if (device_state_ == kDeviceStateIdle) {
        if (persistent_link_enabled_ &&
            (persistent_connecting_.load() || NetworkGenerationChanged(persistent_network_generation_.load(), persistent_network_handled_) || !protocol_->IsAudioChannelOpened())) {
            persistent_pending_wake_ = true;
            persistent_retry_.RetryNow();
            SetDeviceState(kDeviceStateConnecting);
            StartPersistentConnection();
            return;
        }
        audio_service_.EncodeWakeWord();

        if (!protocol_->IsAudioChannelOpened()) {
            SetDeviceState(kDeviceStateConnecting);
            if (!protocol_->OpenAudioChannel()) {
                audio_service_.EnableWakeWordDetection(true);
                return;
            }
        }

        auto wake_word = audio_service_.GetLastWakeWord();
        ESP_LOGI(TAG, "Wake word detected: %s", wake_word.c_str());
#if CONFIG_SEND_WAKE_WORD_DATA
        // Encode and send the wake word data to the server
        while (auto packet = audio_service_.PopWakeWordPacket()) {
            protocol_->SendAudio(std::move(packet));
        }
        // Set the chat state to wake word detected
        protocol_->SendWakeWordDetected(wake_word);
        SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
#else
        SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
        // Play the pop up sound to indicate the wake word is detected
        audio_service_.PlaySound(Lang::Sounds::OGG_POPUP);
#endif
    } else if (device_state_ == kDeviceStateSpeaking) {
        AbortSpeaking(kAbortReasonWakeWordDetected);
    } else if (device_state_ == kDeviceStateActivating) {
        SetDeviceState(kDeviceStateIdle);
    }
}

void Application::AbortSpeaking(AbortReason reason) {
    ESP_LOGI(TAG, "Abort speaking");
    aborted_ = true;
    if (protocol_) {
        protocol_->SendAbortSpeaking(reason);
    }
}

void Application::SetListeningMode(ListeningMode mode) {
    if (Board::GetInstance().IsResting() || rest_waking_) return;
    listening_mode_ = mode;
    SetDeviceState(kDeviceStateListening);
}

void Application::SetDeviceState(DeviceState state) {
    if (state == kDeviceStateListening && (Board::GetInstance().IsResting() || rest_waking_)) return;
    if (state == kDeviceStateIdle && rest_waking_) state = kDeviceStateConnecting;
    if (device_state_ == state) {
        return;
    }
    
    clock_ticks_ = 0;
    auto previous_state = device_state_;
    device_state_ = state;
    ESP_LOGI(TAG, "STATE: %s", STATE_STRINGS[device_state_]);

    // Send the state change event
    DeviceStateEventManager::GetInstance().PostStateChangeEvent(previous_state, state);

    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    auto led = board.GetLed();
    led->OnStateChanged();
    switch (state) {
        case kDeviceStateUnknown:
        case kDeviceStateIdle:
            display->SetStatus(Lang::Strings::STANDBY);
            display->SetEmotion("neutral");
            audio_service_.EnableVoiceProcessing(false);
            audio_service_.EnableWakeWordDetection(!board.IsResting() && !rest_waking_);
            break;
        case kDeviceStateConnecting:
            display->SetStatus(Lang::Strings::CONNECTING);
            display->SetEmotion("neutral");
            display->SetChatMessage("system", "");
            break;
        case kDeviceStateListening: {
            display->SetStatus(Lang::Strings::LISTENING);
            display->SetEmotion("neutral");

            const bool processor_running = audio_service_.IsAudioProcessorRunning();
            // A persistent session needs a new listen_id after every answer,
            // even if Realtime/AEC keeps voice processing active during TTS.
            if (PrepareListeningStart(persistent_link_enabled_, processor_running, persistent_listen_id_)) {
                if (persistent_link_enabled_) {
                    protocol_->SendStartListening(listening_mode_, persistent_listen_id_);
                } else {
                    protocol_->SendStartListening(listening_mode_);
                }
            }
            if (!processor_running) {
                audio_service_.EnableVoiceProcessing(true);
                audio_service_.EnableWakeWordDetection(false);
            }
            break;
        }
        case kDeviceStateSpeaking:
            display->SetStatus(Lang::Strings::SPEAKING);

            if (listening_mode_ != kListeningModeRealtime) {
                audio_service_.EnableVoiceProcessing(false);
                // Only AFE wake word can be detected in speaking mode
                audio_service_.EnableWakeWordDetection(!board.IsResting() && !rest_waking_ && audio_service_.IsAfeWakeWord());
            }
            audio_service_.ResetDecoder();
            break;
        default:
            // Do nothing
            break;
    }
}

void Application::Reboot() {
    ESP_LOGI(TAG, "Rebooting...");
    if (persistent_protocol_ != nullptr) {
        // Restart the whole device without destroying a live transport or
        // waiting for a potentially blocked worker on the event loop.
        esp_restart();
    }
    persistent_link_enabled_ = false;
    ++persistent_epoch_;
    // A background handshake must not access protocol_ after it is destroyed.
    for (int i = 0; i < 200 && persistent_worker_active_.load(); ++i) vTaskDelay(pdMS_TO_TICKS(100));
    if (persistent_worker_active_.load()) esp_restart();
    // Disconnect the audio channel
    if (protocol_ && protocol_->IsAudioChannelOpened()) {
        protocol_->CloseAudioChannel();
    }
    protocol_.reset();
    audio_service_.Stop();

    vTaskDelay(pdMS_TO_TICKS(1000));
    esp_restart();
}

bool Application::UpgradeFirmware(Ota& ota, const std::string& url) {
    // At boot protocol_ does not yet exist. Runtime EchoEar updates must
    // arrive here only after RequestFirmwareUpgrade completed async close.
    if (persistent_protocol_ && !upgrade_close_completed_) {
        ESP_LOGE(TAG, "Firmware upgrade requires asynchronous channel close");
        return false;
    }
    upgrade_close_completed_ = false;
    auto& board = Board::GetInstance();
    auto display = board.GetDisplay();
    
    // Use provided URL or get from OTA object
    std::string upgrade_url = url.empty() ? ota.GetFirmwareUrl() : url;
    std::string version_info = url.empty() ? ota.GetFirmwareVersion() : "(Manual upgrade)";

    const bool restart_persistent = persistent_link_enabled_.exchange(false);
    ++persistent_epoch_;
    for (int i = 0; i < 200 && persistent_worker_active_.load(); ++i) vTaskDelay(pdMS_TO_TICKS(100));
    if (persistent_worker_active_.load()) {
        persistent_link_enabled_ = restart_persistent;
        return false;
    }
    
    // Close audio channel if it's open
    if (!persistent_protocol_ && protocol_ && protocol_->IsAudioChannelOpened()) {
        ESP_LOGI(TAG, "Closing audio channel before firmware upgrade");
        protocol_->CloseAudioChannel();
    }
    ESP_LOGI(TAG, "Starting firmware upgrade");
    
    Alert(Lang::Strings::OTA_UPGRADE, Lang::Strings::UPGRADING, "download", Lang::Sounds::OGG_UPGRADE);
    vTaskDelay(pdMS_TO_TICKS(3000));

    SetDeviceState(kDeviceStateUpgrading);
    
    std::string message = std::string(Lang::Strings::NEW_VERSION) + version_info;
    display->SetChatMessage("system", message.c_str());

    board.SetPowerSaveMode(false);
    audio_service_.Stop();
    vTaskDelay(pdMS_TO_TICKS(1000));

    bool upgrade_success = ota.StartUpgradeFromUrl(upgrade_url, [display](int progress, size_t speed) {
        std::thread([display, progress, speed]() {
            char buffer[32];
            snprintf(buffer, sizeof(buffer), "%d%% %uKB/s", progress, speed / 1024);
            display->SetChatMessage("system", buffer);
        }).detach();
    });

    if (!upgrade_success) {
        // Upgrade failed, restart audio service and continue running
        ESP_LOGE(TAG, "Firmware upgrade failed, restarting audio service and continuing operation...");
        audio_service_.Start(); // Restart audio service
        board.SetPowerSaveMode(true); // Restore power save mode
        SetDeviceState(kDeviceStateIdle);
        if (restart_persistent) {
            Schedule([this]() { persistent_link_enabled_ = true; persistent_retry_.RetryNow(); });
        }
        Alert(Lang::Strings::ERROR, Lang::Strings::UPGRADE_FAILED, "circle_xmark", Lang::Sounds::OGG_EXCLAMATION);
        vTaskDelay(pdMS_TO_TICKS(3000));
        return false;
    } else {
        // Upgrade success, reboot immediately
        ESP_LOGI(TAG, "Firmware upgrade successful, rebooting...");
        display->SetChatMessage("system", "Upgrade successful, rebooting...");
        vTaskDelay(pdMS_TO_TICKS(1000)); // Brief pause to show message
        Reboot();
        return true;
    }
}

bool Application::RequestFirmwareUpgrade(const std::string& url) {
    if (!persistent_protocol_) {
        Ota ota;
        return UpgradeFirmware(ota, url);
    }
    if (upgrade_pending_ || persistent_connecting_.load()) {
        ESP_LOGW(TAG, "Firmware upgrade deferred: network worker is busy");
        return false;
    }
    upgrade_pending_ = true;
    pending_upgrade_url_ = url;
    if (!StartPersistentClose()) {
        upgrade_pending_ = false;
        pending_upgrade_url_.clear();
        return false;
    }
    return true;
}

void Application::WakeWordInvoke(const std::string& wake_word) {
    if (Board::GetInstance().IsResting() || rest_waking_) return;
    if (!protocol_) {
        return;
    }

    if (persistent_protocol_ && persistent_connecting_.load() && !persistent_link_enabled_) return;

    if (persistent_link_enabled_) {
        Schedule([this]() { OnWakeWordDetected(); });
        return;
    }

    if (device_state_ == kDeviceStateIdle) {
        audio_service_.EncodeWakeWord();

        if (!protocol_->IsAudioChannelOpened()) {
            SetDeviceState(kDeviceStateConnecting);
            if (!protocol_->OpenAudioChannel()) {
                audio_service_.EnableWakeWordDetection(true);
                return;
            }
        }

        ESP_LOGI(TAG, "Wake word detected: %s", wake_word.c_str());
#if CONFIG_USE_AFE_WAKE_WORD || CONFIG_USE_CUSTOM_WAKE_WORD
        // Encode and send the wake word data to the server
        while (auto packet = audio_service_.PopWakeWordPacket()) {
            protocol_->SendAudio(std::move(packet));
        }
        // Set the chat state to wake word detected
        protocol_->SendWakeWordDetected(wake_word);
        SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
#else
        SetListeningMode(aec_mode_ == kAecOff ? kListeningModeAutoStop : kListeningModeRealtime);
        // Play the pop up sound to indicate the wake word is detected
        audio_service_.PlaySound(Lang::Sounds::OGG_POPUP);
#endif
    } else if (device_state_ == kDeviceStateSpeaking) {
        Schedule([this]() {
            AbortSpeaking(kAbortReasonNone);
        });
    } else if (device_state_ == kDeviceStateListening) {   
        Schedule([this]() {
            if (protocol_) {
                if (persistent_link_enabled_) {
                    protocol_->SendPauseListening(persistent_listen_id_);
                    if (++persistent_listen_id_ == 0) ++persistent_listen_id_;
                    SetDeviceState(kDeviceStateIdle);
                    audio_service_.ResetDecoder();
                } else {
                    if (persistent_protocol_) StartPersistentClose();
                    else protocol_->CloseAudioChannel();
                }
            }
        });
    }
}

bool Application::CanEnterSleepMode() {
    if (persistent_connecting_.load()) return false;
    if (device_state_ != kDeviceStateIdle) {
        return false;
    }

    if (protocol_ && protocol_->IsAudioChannelOpened()) {
        return false;
    }

    if (!audio_service_.IsIdle()) {
        return false;
    }

    // Now it is safe to enter sleep mode
    return true;
}

void Application::SendMcpMessage(const std::string& payload) {
    if (protocol_ == nullptr) {
        return;
    }

    // Make sure you are using main thread to send MCP message
    if (xTaskGetCurrentTaskHandle() == main_event_loop_task_handle_) {
        if (persistent_link_enabled_ && persistent_connecting_.load()) {
            if (persistent_pending_mcp_.size() < 16) persistent_pending_mcp_.push_back(payload);
        } else {
            protocol_->SendMcpMessage(payload);
        }
    } else {
        Schedule([this, payload = std::move(payload)]() {
            SendMcpMessage(payload);
        });
    }
}

bool Application::CanExecuteRestMcp(uint32_t epoch) const {
    return !persistent_connecting_.load() && persistent_protocol_ && persistent_protocol_->ImageEpoch() == epoch &&
           !persistent_protocol_->TrafficSuspended();
}

void Application::SendRestMcpReply(const std::string& payload, uint32_t epoch) {
    for (int attempt = 0; persistent_protocol_ && attempt < 4; ++attempt) {
        auto result = persistent_protocol_->SendRestMcpReply(payload, epoch);
        if (result == WebsocketProtocol::McpSendResult::Sent) return;
        if (result != WebsocketProtocol::McpSendResult::Busy) break;
        if (attempt < 3) vTaskDelay(pdMS_TO_TICKS(10));
    }
    ESP_LOGW(TAG, "Rest MCP reply unconfirmed");
}

void Application::SendImageBackgroundAck(const char* op, uint32_t id, const char* status, const char* reason, uint32_t epoch) {
    // SendText uses a try-lock, and the receive task may still hold it after
    // queuing this callback. Give it a few brief chances to release the lock.
    for (int attempt = 0; protocol_ && attempt < 4; ++attempt) {
        if (!IsImageEpochCurrent(epoch)) return;
        if (protocol_->SendImageBackgroundAck(op, id, status, reason, epoch)) return;
        if (attempt < 3) vTaskDelay(pdMS_TO_TICKS(10));
    }
    ESP_LOGW(TAG, "Could not send image background acknowledgement");
}

void Application::SetAecMode(AecMode mode) {
    aec_mode_ = mode;
    Schedule([this]() {
        auto& board = Board::GetInstance();
        auto display = board.GetDisplay();
        switch (aec_mode_) {
        case kAecOff:
            audio_service_.EnableDeviceAec(false);
            display->ShowNotification(Lang::Strings::RTC_MODE_OFF);
            break;
        case kAecOnServerSide:
            audio_service_.EnableDeviceAec(false);
            display->ShowNotification(Lang::Strings::RTC_MODE_ON);
            break;
        case kAecOnDeviceSide:
            audio_service_.EnableDeviceAec(true);
            display->ShowNotification(Lang::Strings::RTC_MODE_ON);
            break;
        }

        // If the AEC mode is changed, close the audio channel
        if (protocol_ && !persistent_connecting_.load() && protocol_->IsAudioChannelOpened()) {
            if (persistent_protocol_) StartPersistentClose();
            else protocol_->CloseAudioChannel();
        }
    });
}

void Application::PlaySound(const std::string_view& sound) {
    audio_service_.PlaySound(sound);
}

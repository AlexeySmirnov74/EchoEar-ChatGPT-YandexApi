#ifndef _WEBSOCKET_PROTOCOL_H_
#define _WEBSOCKET_PROTOCOL_H_


#include "protocol.h"
#include "connection_state.h"

#include <web_socket.h>
#include <freertos/FreeRTOS.h>
#include <freertos/event_groups.h>
#include <atomic>
#include <mutex>

#define WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT (1 << 0)

class WebsocketProtocol : public Protocol {
public:
    explicit WebsocketProtocol(const std::atomic_bool* persistent_idle_enabled = nullptr);
    ~WebsocketProtocol();

    bool Start() override;
    bool SendAudio(std::unique_ptr<AudioStreamPacket> packet) override;
    bool OpenAudioChannel() override;
    void CloseAudioChannel() override;
    bool IsAudioChannelOpened() const override;
    bool SupportsPersistentIdle() const { return persistent_idle_supported_.load(); }
    // Nonblocking alarm barrier; cannot revoke a write already in flight.
    void SuspendTraffic() { traffic_suspended_.store(true); }
    bool TrafficSuspended() const { return traffic_suspended_.load(); }
    using AudioChannelState = ::AudioChannelState;
    AudioChannelState TryGetAudioChannelState() const;
    uint32_t ImageEpoch() const override { return socket_generation_.load(); }
    enum class McpSendResult { Sent, Busy, Failed };
    McpSendResult SendRestMcpReply(const std::string& payload, uint32_t epoch);

private:
    EventGroupHandle_t event_group_handle_;
    std::unique_ptr<WebSocket> websocket_;
    mutable std::recursive_mutex websocket_mutex_;
    int version_ = 1;
    const std::atomic_bool* const persistent_idle_enabled_;
    std::atomic_bool persistent_idle_supported_{false};
    std::atomic_bool traffic_suspended_{false};
    std::atomic<int64_t> ws_last_incoming_us_{0};
    std::atomic<uint32_t> socket_generation_{0};

    void ParseServerHello(const cJSON* root);
    bool SendText(const std::string& text) override;
    bool SendTextForImageEpoch(const std::string& text, uint32_t epoch) override;
    std::string GetHelloMessage();
};

#endif

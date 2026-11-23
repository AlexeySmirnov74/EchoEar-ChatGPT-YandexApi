#include <atomic>
#include <cassert>
#include <chrono>
#include <cstdint>
#include <mutex>
#include <string>
#include <thread>
#include <iostream>
struct Socket {
    bool connected = true, succeeds = true;
    int sends = 0;
    bool IsConnected() const { return connected; }
    bool Send(const std::string&) { ++sends; return succeeds; }
};
struct WebsocketProtocol {
    enum class McpSendResult { Sent, Busy, Failed };
    std::recursive_mutex websocket_mutex_;
    std::atomic<uint32_t> socket_generation_{1};
    std::atomic<bool> traffic_suspended_{false};
    Socket* websocket_ = nullptr;
    std::string session_id_ = "session";
    uint32_t ImageEpoch() const { return socket_generation_.load(); }
    bool TrafficSuspended() const { return traffic_suspended_.load(); }
    McpSendResult SendRestMcpReply(const std::string&, uint32_t);
};
struct Application {
    WebsocketProtocol* persistent_protocol_ = nullptr;
    std::atomic<bool> persistent_connecting_{false};
    bool CanExecuteRestMcp(uint32_t) const;
    void SendRestMcpReply(const std::string&, uint32_t);
};
#define pdMS_TO_TICKS(x) (x)
#define vTaskDelay(x) std::this_thread::sleep_for(std::chrono::milliseconds(x))
#define ESP_LOGW(...) ((void)0)

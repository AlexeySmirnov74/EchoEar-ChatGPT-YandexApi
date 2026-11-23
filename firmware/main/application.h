#ifndef _APPLICATION_H_
#define _APPLICATION_H_

#include <freertos/FreeRTOS.h>
#include <freertos/event_groups.h>
#include <freertos/task.h>
#include <esp_timer.h>

#include <string>
#include <mutex>
#include <deque>
#include <memory>
#include <atomic>

#include "protocol.h"
#include "ota.h"
#include "audio_service.h"
#include "wake_ready_policy.h"
#include "device_state_event.h"
#include "persistent_link_policy.h"
#include "async_close_policy.h"
#include "alarm_store.h"


#define MAIN_EVENT_SCHEDULE (1 << 0)
#define MAIN_EVENT_SEND_AUDIO (1 << 1)
#define MAIN_EVENT_WAKE_WORD_DETECTED (1 << 2)
#define MAIN_EVENT_VAD_CHANGE (1 << 3)
#define MAIN_EVENT_ERROR (1 << 4)
#define MAIN_EVENT_CHECK_NEW_VERSION_DONE (1 << 5)
#define MAIN_EVENT_CLOCK_TICK (1 << 6)
#define MAIN_EVENT_ASYNC_CLOSE_DONE (1 << 7)
#define MAIN_EVENT_WIFI_CHANGED (1 << 8)


enum AecMode {
    kAecOff,
    kAecOnDeviceSide,
    kAecOnServerSide,
};

class WebsocketProtocol;

class Application {
public:
    static Application& GetInstance() {
        static Application instance;
        return instance;
    }
    // 删除拷贝构造函数和赋值运算符
    Application(const Application&) = delete;
    Application& operator=(const Application&) = delete;

    void Start();
    void MainEventLoop();
    DeviceState GetDeviceState() const { return device_state_; }
    bool IsVoiceDetected() const { return audio_service_.IsVoiceDetected(); }
    void Schedule(std::function<void()> callback);
    void SetDeviceState(DeviceState state);
    void Alert(const char* status, const char* message, const char* emotion = "", const std::string_view& sound = "");
    void DismissAlert();
    void AbortSpeaking(AbortReason reason);
    void ToggleChatState();
    void HandleEchoEarTouch();
    void RequestTouchFeedback() { (void)audio_service_.RequestTouchFeedback(); }
    void StartListening();
    void StopListening();
    void Reboot();
    void WakeWordInvoke(const std::string& wake_word);
    bool UpgradeFirmware(Ota& ota, const std::string& url = "");
    bool RequestFirmwareUpgrade(const std::string& url);
    bool CanEnterSleepMode();
    void SendMcpMessage(const std::string& payload);
    void SendRestMcpReply(const std::string& payload, uint32_t epoch);
    bool CanExecuteRestMcp(uint32_t epoch) const;
    void SendImageBackgroundAck(const char* op, uint32_t id, const char* status, const char* reason, uint32_t epoch);
    bool IsImageEpochCurrent(uint32_t epoch) const { return protocol_ && protocol_->ImageEpoch() == epoch; }
    void SetAecMode(AecMode mode);
    AecMode GetAecMode() const { return aec_mode_; }
    void PlaySound(const std::string_view& sound);
    AudioService& GetAudioService() { return audio_service_; }
    std::string SetAlarm(int hour, int minute, int day, bool daily);
    std::string SetAlarmAfter(int minutes);
    std::string DeleteAlarm(uint32_t id);
    std::string ListAlarms() const;
    bool SetAlarmVolumeFloor(int floor);
    // Called on the application event loop by the board's rest MCP tools.
    bool SetRestMode(bool enabled);
    bool IsAlarmRinging() const { return ringing_alarm_id_.load() != 0; }
    bool IsRestWaking() const { return rest_waking_; }

private:
    Application();
    ~Application();

    std::mutex mutex_;
    std::deque<std::function<void()>> main_tasks_;
    std::unique_ptr<Protocol> protocol_;
    EventGroupHandle_t event_group_ = nullptr;
    esp_timer_handle_t clock_timer_handle_ = nullptr;
    esp_timer_handle_t touch_ready_timer_handle_ = nullptr;
    std::atomic<bool> touch_waiting_to_listen_{false};
    std::atomic<bool> touch_feedback_started_{false};
    int64_t touch_waiting_since_us_ = 0; // application event loop only
    volatile DeviceState device_state_ = kDeviceStateUnknown;
    ListeningMode listening_mode_ = kListeningModeAutoStop;
    AecMode aec_mode_ = kAecOff;
    std::string last_error_message_;
    AudioService audio_service_;

    bool has_server_time_ = false;
    AlarmStore alarm_store_;
    std::atomic<uint32_t> ringing_alarm_id_{0};
    int64_t last_alarm_sound_us_ = 0;
    int64_t last_alarm_diag_us_ = 0;
    int alarm_volume_floor_ = 70; // 0 means use the current speaker volume
    void PollAlarm();
    bool DismissAlarmForTouch();
    bool aborted_ = false;
    int clock_ticks_ = 0;
    TaskHandle_t check_new_version_task_handle_ = nullptr;
    TaskHandle_t main_event_loop_task_handle_ = nullptr;

    // Experimental EchoEar-2ST persistent-transport draft. The retry policy
    // and pending actions are only mutated on the application event loop.
    std::atomic_bool persistent_link_enabled_{false};
    WebsocketProtocol* persistent_protocol_ = nullptr;
    std::atomic_bool persistent_connecting_{false};
    std::atomic_bool persistent_worker_active_{false};
    std::atomic<uint32_t> persistent_epoch_{0};
    bool persistent_pending_listen_ = false;
    ListeningMode persistent_pending_mode_ = kListeningModeAutoStop;
    bool persistent_pending_wake_ = false;
    bool persistent_wifi_was_connected_ = false;
    std::atomic<uint32_t> persistent_network_generation_{0};
    uint32_t persistent_network_handled_ = 0;
    bool persistent_network_events_ready_ = false;
    uint32_t persistent_listen_id_ = 0;
    PersistentLinkPolicy persistent_retry_;
    bool persistent_refresh_ota_ = false;
    bool legacy_cleanup_retry_ = false; // main event loop only
    bool legacy_cleanup_started_ = false;
    int64_t legacy_cleanup_next_us_ = 0;
    AsyncClosePolicy async_close_;
    bool upgrade_pending_ = false;
    bool rest_close_pending_ = false;
    bool rest_transport_retired_ = false;
    bool rest_connection_lost_ = false;
    bool rest_close_timeout_reported_ = false;
    bool rest_tts_started_ = false;
    bool rest_tts_finished_ = false;
    int64_t rest_started_us_ = 0;
    int64_t rest_drained_since_us_ = 0;
    std::atomic_bool rest_waking_{false};
    echowake::Phase rest_wake_phase_ = echowake::Phase::First;
    uint32_t rest_wake_audio_generation_ = 0;
    uint32_t rest_wake_socket_generation_ = 0;
    uint32_t rest_wake_network_generation_ = 0;
    int64_t rest_wake_drained_since_us_ = 0;
    std::atomic_bool rest_wake_prompt_feeding_{false};
    bool StartRestWakePrompt(bool ready);
    void PollRestConnection();
    void PollRestWake();
    bool upgrade_close_completed_ = false;
    std::string pending_upgrade_url_;
    std::deque<std::string> persistent_pending_mcp_;

    void StartPersistentConnection();
    bool StartPersistentClose();
    void PollPersistentClose();
    void ProcessPersistentNetworkChange();

    void OnWakeWordDetected();
    void CheckNewVersion(Ota& ota);
    void CheckAssetsVersion();
    void ShowActivationCode(const std::string& code, const std::string& message);
    void SetListeningMode(ListeningMode mode);
};


class TaskPriorityReset {
public:
    TaskPriorityReset(BaseType_t priority) {
        original_priority_ = uxTaskPriorityGet(NULL);
        vTaskPrioritySet(NULL, priority);
    }
    ~TaskPriorityReset() {
        vTaskPrioritySet(NULL, original_priority_);
    }

private:
    BaseType_t original_priority_;
};

#endif // _APPLICATION_H_

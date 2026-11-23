#include <cassert>
#include <cstdint>
#include <cstdio>
#include <mutex>
#include <thread>
#include <vector>
#include <atomic>
#include <chrono>
static int saved_brightness = -1;
static std::atomic_bool timer_active{false};
static std::atomic_bool fail_next_start{false};
class Settings { public: Settings(const char*, bool) {} void SetInt(const char*, int x){saved_brightness=x;} };
constexpr int ESP_OK = 0;
static int esp_timer_start_periodic(void*, int) {
    if (fail_next_start.exchange(false)) return -1;
    timer_active = true; return ESP_OK;
}
static void esp_timer_stop(void*) { timer_active = false; }
#define ESP_LOGI(...) do {} while(0)
class Backlight {
public:
  void SetBrightness(uint8_t brightness, bool permanent = false);
  bool SetBlanked(bool blanked);
  void OnTransitionTimer();
  void SetBrightnessImpl(uint8_t b) { output=b; }
  uint8_t brightness_ = 50, target_brightness_ = 50, selected_brightness_=50, step_ = 1, output=50;
  bool blanked_=false;
  void* transition_timer_ = this;
  mutable std::mutex state_mutex_;
  std::mutex command_mutex_;
  uint8_t brightness() const { std::lock_guard<std::mutex> lock(state_mutex_); return brightness_; }
};

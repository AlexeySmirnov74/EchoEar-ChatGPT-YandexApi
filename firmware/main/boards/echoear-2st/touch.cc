#include "touch.h"
#include "config.h"

#include <esp_check.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

// Register 2 format and measured address documented by the EchoEar-2ST port:
// https://github.com/facebookincubator/muse-gadget-sdk/blob/main/esp32/devices/ostb-echoear-2st.md
namespace {
// Poll frequently enough to catch short taps while yielding between I2C reads.
// A delay of at least one tick also avoids a busy loop at lower tick rates.
constexpr TickType_t kTouchPollDelay =
    pdMS_TO_TICKS(10) > 0 ? pdMS_TO_TICKS(10) : 1;

struct TouchState {
    i2c_master_dev_handle_t device;
    void (*on_release)(void*);
    void* context;
};

void PollTouch(void* arg)
{
    auto* state = static_cast<TouchState*>(arg);
    bool was_pressed = false;
    for (;;) {
        uint8_t reg = 2;
        uint8_t raw[6] = {};
        const auto result = i2c_master_transmit_receive(state->device, &reg, 1,
                                                         raw, sizeof(raw), 20);
        if (result != ESP_OK) {
            // A failed transfer is not a release: avoid toggling chat on errors.
            was_pressed = false;
        } else {
            const unsigned count = raw[0] & 0x0f;
            const unsigned x = ((raw[1] & 0x0f) << 8) | raw[2];
            const unsigned y = ((raw[3] & 0x0f) << 8) | raw[4];
            if (count == 0) {
                if (was_pressed) {
                    was_pressed = false;
                    state->on_release(state->context);
                }
            } else if (count == 1 && x < ECHOEAR_LCD_WIDTH && y < ECHOEAR_LCD_HEIGHT) {
                was_pressed = true;
            } else {
                // Discard multi-touch and invalid positions, including release.
                was_pressed = false;
            }
        }
        vTaskDelay(kTouchPollDelay);
    }
}
}  // namespace

void StartEchoEar2stTouch(i2c_master_bus_handle_t bus,
                         void (*on_release)(void*), void* context)
{
    ESP_ERROR_CHECK(on_release ? ESP_OK : ESP_ERR_INVALID_ARG);
    i2c_device_config_t device_config = {};
    device_config.dev_addr_length = I2C_ADDR_BIT_LEN_7;
    device_config.device_address = ECHOEAR_TOUCH_ADDR;
    device_config.scl_speed_hz = 400000;
    auto* state = new TouchState{};
    state->on_release = on_release;
    state->context = context;
    ESP_ERROR_CHECK(i2c_master_bus_add_device(bus, &device_config, &state->device));
    ESP_ERROR_CHECK(i2c_master_probe(bus, ECHOEAR_TOUCH_ADDR, 100));
    const auto result = xTaskCreate(PollTouch, "echoear_touch", 4096, state, 5, nullptr);
    ESP_ERROR_CHECK(result == pdPASS ? ESP_OK : ESP_ERR_NO_MEM);
}

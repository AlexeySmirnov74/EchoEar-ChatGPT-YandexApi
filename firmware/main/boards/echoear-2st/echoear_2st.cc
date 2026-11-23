#include "wifi_board.h"
#include "codecs/box_audio_codec.h"
#include "display/display.h"
#include "application.h"
#include "backlight.h"
#include "config.h"
#include "display.h"
#include "touch.h"
#include "esp_battery_probe.h"
#include "mcp_server.h"

#include <driver/i2c_master.h>
#include <esp_check.h>
#include <esp_codec_dev_defaults.h>
#include <wifi_station.h>

// In development: 2ST-specific GPIOs and board initialization are based on
// the documented OSTB EchoEar-2ST profile, not generic EchoEar or ESP-VoCat.
// No image built from this source has been validated on the target hardware.
class EchoEar2stBoard : public WifiBoard {
private:
    i2c_master_bus_handle_t i2c_bus_ = nullptr;
    Display* display_ = nullptr;
    Backlight* backlight_ = nullptr;
    bool resting_ = false;  // Accessed only on the application event loop.
    echoear_battery_lab::EspBatteryProbe battery_probe_;

    static void OnTouchRelease(void* context) {
        auto* board = static_cast<EchoEar2stBoard*>(context);
        auto& app = Application::GetInstance();
        app.Schedule([board, &app]() {
            if (app.IsAlarmRinging()) {
                app.HandleEchoEarTouch();
                return;
            }
            if (app.IsRestWaking()) { app.RequestTouchFeedback(); return; }
            // This release wakes only. A following touch starts conversation.
            if (board->IsResting()) {
                app.RequestTouchFeedback();
                if (!app.SetRestMode(false)) ESP_LOGE("EchoEarRest", "Touch wake failed");
                return;
            }
            if (app.GetDeviceState() == kDeviceStateStarting &&
                !WifiStation::GetInstance().IsConnected()) {
                app.RequestTouchFeedback();
                board->ResetWifiConfiguration();
            } else {
                app.HandleEchoEarTouch();
            }
        });
    }

    void RegisterRestTools() {
        auto response = [this](bool success) -> ReturnValue {
            cJSON* result = cJSON_CreateObject();
            if (!result) throw std::runtime_error("rest status allocation failed");
            cJSON_AddBoolToObject(result, "success", success);
            cJSON_AddStringToObject(result, "mode", resting_ ? "resting" : "awake");
            return result;
        };
        McpServer::GetInstance().AddTool("self.rest.enter",
            "Enter display rest; the device remains powered and alarms work. This is not power-off.",
            PropertyList(), [response](const PropertyList&) -> ReturnValue {
                return response(Application::GetInstance().SetRestMode(true));
            });
        McpServer::GetInstance().AddTool("self.rest.wake",
            "Leave display rest. This does not start a conversation.",
            PropertyList(), [response](const PropertyList&) -> ReturnValue {
                return response(Application::GetInstance().SetRestMode(false));
            });
        McpServer::GetInstance().AddTool("self.rest.status",
            "Get the current display rest mode.", PropertyList(),
            [response](const PropertyList&) -> ReturnValue { return response(true); });
    }

public:
    EchoEar2stBoard() {
        i2c_master_bus_config_t cfg = {};
        cfg.i2c_port = I2C_NUM_0;
        cfg.sda_io_num = AUDIO_CODEC_I2C_SDA_PIN;
        cfg.scl_io_num = AUDIO_CODEC_I2C_SCL_PIN;
        cfg.clk_source = I2C_CLK_SRC_DEFAULT;
        cfg.glitch_ignore_cnt = 7;
        cfg.flags.enable_internal_pullup = true;
        ESP_ERROR_CHECK(i2c_new_master_bus(&cfg, &i2c_bus_));

        display_ = CreateEchoEar2stDisplay();
        backlight_ = CreateEchoEar2stBacklight();
        battery_probe_.RegisterTool();
        RegisterRestTools();
        StartEchoEar2stTouch(i2c_bus_, OnTouchRelease, this);
    }

    AudioCodec* GetAudioCodec() override {
        // The current BoxAudioCodec uses four-slot ES7210 input, matching the
        // factory boot log. The reference channel must be confirmed separately.
        static BoxAudioCodec codec(i2c_bus_, AUDIO_INPUT_SAMPLE_RATE,
            AUDIO_OUTPUT_SAMPLE_RATE, AUDIO_I2S_GPIO_MCLK, AUDIO_I2S_GPIO_BCLK,
            AUDIO_I2S_GPIO_WS, AUDIO_I2S_GPIO_DOUT, AUDIO_I2S_GPIO_DIN,
            AUDIO_CODEC_PA_PIN, ES8311_CODEC_DEFAULT_ADDR,
            ES7210_CODEC_DEFAULT_ADDR, AUDIO_INPUT_REFERENCE);
        return &codec;
    }

    Display* GetDisplay() override { return display_; }
    Backlight* GetBacklight() override { return backlight_; }
    bool IsResting() const override { return resting_; }
    bool SetRestMode(bool enabled) override {
        if (resting_ == enabled) return true;
        if (!backlight_ || !backlight_->SetBlanked(enabled)) return false;
        resting_ = enabled;
        return true;
    }
    bool SetRestAlarmVisible(bool visible) override {
        return resting_ && backlight_ && backlight_->SetBlanked(!visible);
    }
};

DECLARE_BOARD(EchoEar2stBoard);

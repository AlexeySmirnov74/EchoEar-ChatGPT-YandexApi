#pragma once
// Prototype only. The common board status cannot express charging as unknown.
#include <mutex>
#include <esp_adc/adc_oneshot.h>
#include <esp_log.h>
#include "mcp_server.h"
#include "battery_sample.h"

namespace echoear_battery_lab {
class EspBatteryProbe {
public:
    EspBatteryProbe(const EspBatteryProbe&) = delete;
    EspBatteryProbe& operator=(const EspBatteryProbe&) = delete;
    EspBatteryProbe() = default;
    void RegisterTool() {
        McpServer::GetInstance().AddTool(
            "self.battery.get_estimate",
            "Get an approximate battery percentage. The measurement is uncalibrated; "
            "charging state is unknown. If unavailable, do not invent a percentage.",
            PropertyList(), [this](const PropertyList&) -> ReturnValue { return Read(); });
    }
private:
    std::mutex mutex_;
    adc_oneshot_unit_handle_t handle_ = nullptr;
    static constexpr const char* kTag = "BatteryLab";
    cJSON* Read() {
        std::lock_guard<std::mutex> lock(mutex_);
        cJSON* json = cJSON_CreateObject();
        if (!handle_) {
            adc_oneshot_unit_init_cfg_t unit{};
            unit.unit_id = ADC_UNIT_2;
            esp_err_t err = adc_oneshot_new_unit(&unit, &handle_);
            if (err == ESP_OK) {
                adc_oneshot_chan_cfg_t channel{};
                channel.atten = ADC_ATTEN_DB_12;
                channel.bitwidth = ADC_BITWIDTH_12;
                err = adc_oneshot_config_channel(handle_, ADC_CHANNEL_6, &channel);
                if (err != ESP_OK) {
                    adc_oneshot_del_unit(handle_);
                    handle_ = nullptr;
                }
            }
            if (err != ESP_OK) {
                ESP_LOGW(kTag, "ADC init unavailable err=%d", static_cast<int>(err));
                cJSON_AddBoolToObject(json, "available", false);
                return json;
            }
        }
        int mean = 0, percent = 0, successful = 0;
        const bool ok = Sample([this](int& raw) {
            return adc_oneshot_read(handle_, ADC_CHANNEL_6, &raw) == ESP_OK;
        }, mean, percent, successful);
        if (!ok) {
            ESP_LOGW(kTag, "ADC reading unavailable samples=%d/16", successful);
            cJSON_AddBoolToObject(json, "available", false);
            return json;
        }
        ESP_LOGI(kTag, "ADC average=%d samples=%d/16 estimate=%d", mean, successful, percent);
        cJSON_AddBoolToObject(json, "available", true);
        cJSON_AddNumberToObject(json, "estimated_percent", percent);
        cJSON_AddStringToObject(json, "charging_status", "unknown");
        cJSON_AddBoolToObject(json, "approximate", true);
        return json;
    }
};
}

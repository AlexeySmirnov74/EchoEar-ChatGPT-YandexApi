#include <esp_log.h>
#include <esp_err.h>
#include <nvs.h>
#include <nvs_flash.h>
#include <driver/gpio.h>
#include <esp_event.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>

#include "application.h"
#include "system_info.h"

#define TAG "main"

extern "C" void app_main(void)
{
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
#if !CONFIG_LOG_DYNAMIC_LEVEL_CONTROL
#error "EchoEar requires dynamic log level control to protect portal credentials"
#endif
    // The upstream portal logs the full custom URL and SmartConfig password at
    // INFO. Set its level before any board or portal initialization.
    esp_log_level_set("WifiConfigurationAp", ESP_LOG_WARN);
#endif
    // Initialize the default event loop
    ESP_ERROR_CHECK(esp_event_loop_create_default());

    // Initialize NVS flash for WiFi configuration
    esp_err_t ret = nvs_flash_init();
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    if (ret != ESP_OK) {
        // Preserve Wi-Fi, alarms and all other settings for recovery.
        ESP_LOGE(TAG, "NVS initialization failed; settings were preserved");
        return;
    }
#else
    if (ret == ESP_ERR_NVS_NO_FREE_PAGES || ret == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_LOGW(TAG, "Erasing NVS flash to fix corruption");
        ESP_ERROR_CHECK(nvs_flash_erase());
        ret = nvs_flash_init();
    }
    ESP_ERROR_CHECK(ret);
#endif

    // Launch the application
    auto& app = Application::GetInstance();
    app.Start();
}

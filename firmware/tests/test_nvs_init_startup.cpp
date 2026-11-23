#include "application.h"
#include "esp_err.h"
#include <cassert>

extern "C" void app_main();
static esp_err_t initial_result;
static int initializations, erases, starts;

esp_err_t esp_event_loop_create_default() { return ESP_OK; }
void esp_log_level_set(const char*, int) {}
esp_err_t nvs_flash_init() { return ++initializations == 1 ? initial_result : ESP_OK; }
esp_err_t nvs_flash_erase() { ++erases; return ESP_OK; }
Application& Application::GetInstance() { static Application app; return app; }
void Application::Start() { ++starts; }

static void run(esp_err_t result, int want_init, int want_erase, int want_start) {
    initial_result=result; initializations=erases=starts=0;
    app_main();
    assert(initializations == want_init);
    assert(erases == want_erase);
    assert(starts == want_start);
}

int main() {
    run(ESP_OK, 1, 0, 1);
#if CONFIG_BOARD_TYPE_ECHOEAR_2ST
    run(ESP_ERR_NVS_NO_FREE_PAGES, 1, 0, 0);
    run(ESP_ERR_NVS_NEW_VERSION_FOUND, 1, 0, 0);
    run(99, 1, 0, 0);
#else
    run(ESP_ERR_NVS_NO_FREE_PAGES, 2, 1, 1);
    run(ESP_ERR_NVS_NEW_VERSION_FOUND, 2, 1, 1);
#endif
}

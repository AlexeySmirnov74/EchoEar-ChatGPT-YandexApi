#pragma once
using esp_err_t = int;
constexpr esp_err_t ESP_OK = 0;
constexpr esp_err_t ESP_ERR_NVS_NO_FREE_PAGES = 1;
constexpr esp_err_t ESP_ERR_NVS_NEW_VERSION_FOUND = 2;
#define ESP_ERROR_CHECK(expr) do { if ((expr) != ESP_OK) throw 1; } while (0)

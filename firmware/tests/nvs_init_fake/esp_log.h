#pragma once
constexpr int ESP_LOG_WARN = 2;
void esp_log_level_set(const char*, int);
#define ESP_LOGE(...) ((void)0)
#define ESP_LOGW(...) ((void)0)

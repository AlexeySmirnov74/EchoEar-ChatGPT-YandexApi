#pragma once
#include <cstddef>
using nvs_handle_t = unsigned;
constexpr int ESP_OK=0, ESP_ERR_NVS_NOT_FOUND=1, ESP_FAIL=2;
constexpr int NVS_READONLY=0, NVS_READWRITE=1;
int nvs_open(const char*, int, nvs_handle_t*);
int nvs_get_str(nvs_handle_t, const char*, char*, size_t*);
int nvs_erase_key(nvs_handle_t, const char*);
int nvs_commit(nvs_handle_t);
void nvs_close(nvs_handle_t);

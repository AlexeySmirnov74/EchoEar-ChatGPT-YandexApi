#pragma once
#include "legacy_mqtt_cleanup.h"
#include <nvs.h>

class LegacyMqttNvs {
    nvs_handle_t handle_ = 0;
public:
    legacy_cleanup::Read ReadEndpoint(std::string& out) {
        nvs_handle_t read_handle = 0;
        const auto opened = nvs_open("mqtt", NVS_READONLY, &read_handle);
        if (opened == ESP_ERR_NVS_NOT_FOUND) return legacy_cleanup::Read::Missing;
        if (opened != ESP_OK) return legacy_cleanup::Read::Error;
        size_t size = 0;
        auto err = nvs_get_str(read_handle, "endpoint", nullptr, &size);
        if (err == ESP_ERR_NVS_NOT_FOUND) { nvs_close(read_handle); return legacy_cleanup::Read::Missing; }
        if (err != ESP_OK || !size || size > 256) { nvs_close(read_handle); return legacy_cleanup::Read::Error; }
        out.resize(size);
        err = nvs_get_str(read_handle, "endpoint", out.data(), &size);
        nvs_close(read_handle);
        if (err != ESP_OK || out.empty() || out.back() != '\0') return legacy_cleanup::Read::Error;
        out.pop_back();
        return legacy_cleanup::Read::Found;
    }
    bool Begin() { return nvs_open("mqtt", NVS_READWRITE, &handle_) == ESP_OK; }
    bool Erase(const char* key) {
        const auto result = nvs_erase_key(handle_, key);
        return result == ESP_OK || result == ESP_ERR_NVS_NOT_FOUND;
    }
    bool Commit() { return nvs_commit(handle_) == ESP_OK; }
    void End() { if (handle_) { nvs_close(handle_); handle_ = 0; } }
    bool Verify() {
        nvs_handle_t check = 0;
        const auto result = nvs_open("mqtt", NVS_READONLY, &check);
        if (result == ESP_ERR_NVS_NOT_FOUND) return true;
        if (result != ESP_OK) return false;
        bool cleared = true;
        for (const char* key : {"password", "username", "client_id", "publish_topic", "subscribe_topic", "endpoint"}) {
            size_t size = 0;
            if (nvs_get_str(check, key, nullptr, &size) != ESP_ERR_NVS_NOT_FOUND) cleared = false;
        }
        nvs_close(check);
        return cleared;
    }
};

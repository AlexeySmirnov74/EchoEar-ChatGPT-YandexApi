#pragma once
#include <cassert>
#include <string>

#define CONFIG_OTA_URL "https://fallback.example:18444/base"
inline std::string fake_ota_url;
class Settings {
public:
    Settings(const std::string& name, bool writable) { assert(name == "wifi" && !writable); }
    std::string GetString(const std::string& name) { assert(name == "ota_url"); return fake_ota_url; }
};

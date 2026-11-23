#pragma once
#include "endpoint_policy.h"
#include "settings.h"

inline std::string GetDeploymentOtaUrl() {
    Settings wifi("wifi", false);
    return endpoint_policy::DeploymentUrl(wifi.GetString("ota_url"), CONFIG_OTA_URL);
}

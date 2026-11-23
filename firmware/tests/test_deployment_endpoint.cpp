#include "deployment_endpoint.h"
#include <cassert>
#include <iostream>

int main() {
    fake_ota_url.clear();
    assert(GetDeploymentOtaUrl() == "https://fallback.example:18444/base");
    fake_ota_url = "https://personal.example:18444/private";
    assert(GetDeploymentOtaUrl() == fake_ota_url);
    assert(endpoint_policy::AllowedWebsocket(GetDeploymentOtaUrl(), "wss://personal.example:18444/ws"));
    assert(!endpoint_policy::AllowedWebsocket(GetDeploymentOtaUrl(), "wss://fallback.example:18444/ws"));
    fake_ota_url = "http://personal.example/unsafe";
    assert(GetDeploymentOtaUrl().empty());
    fake_ota_url = "https://api.tenclass.net/old";
    assert(GetDeploymentOtaUrl().empty());
    std::cout << "portal OTA setting and endpoint binding passed\n";
}

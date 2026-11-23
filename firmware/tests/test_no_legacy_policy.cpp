#include "../main/endpoint_policy.h"
#include "../main/legacy_mqtt_cleanup.h"
#include "../main/persistent_link_policy.h"
#include <cassert>
#include <map>
#include <string>
#include <iostream>
using namespace legacy_cleanup;
struct FakeNvs {
    std::map<std::string,std::string> data;
    bool fail_read=false, fail_open=false, fail_commit=false, fail_verify=false;
    int fail_erase=-1, erase_count=0;
    Read ReadEndpoint(std::string& out) {
        if (fail_read) return Read::Error;
        const auto it=data.find("endpoint"); if(it==data.end()) return Read::Missing;
        out=it->second; return Read::Found;
    }
    bool Begin() { return !fail_open; }
    bool Erase(const char* key) { if (erase_count++ == fail_erase) return false; data.erase(key); return true; }
    bool Commit() { return !fail_commit; }
    void End() {}
    bool Verify() {
        if (fail_verify) return false;
        for (const char* key : {"password", "username", "client_id", "publish_topic", "subscribe_topic", "endpoint"})
            if (data.count(key)) return false;
        return true;
    }
};
int main() {
    const std::string ota="https://echo.example.com:18444/private";
    auto good=[&](const char* s) { return endpoint_policy::AllowedWebsocket(ota,s); };
    assert(good("wss://echo.example.com:18444/ws"));
    assert(good("wss://ECHO.EXAMPLE.COM:18444/ws"));
    assert(!good("WSS://echo.example.com:18444/ws")); // transport requires lowercase scheme
    assert(!good("ws://echo.example.com:18444/ws"));
    assert(!good("wss://echo.example.com.evil:18444/ws"));
    assert(!good("wss://evil-echo.example.com:18444/ws"));
    assert(!good("wss://echo.example.com:443/ws"));
    assert(!good("wss://echo.example.com:018444/ws"));
    assert(!good("wss://echo.example.com:18444@evil/ws"));
    assert(!good("wss://user@echo.example.com:18444/ws"));
    assert(!good("wss://echo.example.com:18444?x/ws"));
    assert(!good("wss://echo.example.com:18444\\@evil/ws"));
    assert(!endpoint_policy::AllowedWebsocket("http://echo.example.com/a", "wss://echo.example.com/ws"));
    assert(endpoint_policy::AllowedWebsocket("https://echo.example.com/a", "wss://echo.example.com:443/ws"));
    assert(endpoint_policy::AllowedUpgrade(ota, "https://ECHO.EXAMPLE.COM:18444/fw"));
    assert(!endpoint_policy::AllowedUpgrade(ota, "http://echo.example.com:18444/fw"));
    assert(!endpoint_policy::AllowedUpgrade(ota, "https://echo.example.com.evil:18444/fw"));
    assert(!endpoint_policy::AllowedUpgrade(ota, "https://user@echo.example.com:18444/fw"));
    assert(!endpoint_policy::AllowedUpgrade(ota, "https://echo.example.com:443/fw"));
    assert(!endpoint_policy::AllowedExternalHttp("https://api.TENCLASS.net/a"));
    assert(!endpoint_policy::AllowedExternalHttp("http://tenclass.net/a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://a.b.xiaozhi.me:443/a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://xiaozhi.me./a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://user@allowed.example/a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://api%2etenclass.net/a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://api.tenclass.net@allowed.example/a"));
    assert(!endpoint_policy::AllowedExternalHttp("https://allowed.example:bad/a"));
    assert(!endpoint_policy::AllowedExternalHttp("ftp://allowed.example/a"));
    assert(endpoint_policy::AllowedExternalHttp("https://allowed.example/a?site=tenclass.net"));
    assert(endpoint_policy::AllowedExternalHttp("HTTP://nottenclass.net:80/a"));
    assert(endpoint_policy::AllowedExternalHttp("https://tenclass.net.example/a"));
    assert(endpoint_policy::AcceptUpgradeStatus(200));
    for (int code : {301,302,303,307,308}) assert(!endpoint_policy::AcceptUpgradeStatus(code));
    assert(!endpoint_policy::NeedOtaRefresh(false, ota, "wss://echo.example.com:18444/ws"));
    assert(endpoint_policy::NeedOtaRefresh(true, ota, "wss://echo.example.com:18444/ws"));
    assert(endpoint_policy::NeedOtaRefresh(false, ota, "wss://api.tenclass.net/ws"));
    assert(endpoint_policy::NeedOtaRefresh(false, ota, ""));
    const std::string personal = "https://home.example:18444/token";
    const std::string alternate = "https://home.example:18444/different-token";
    assert(endpoint_policy::DeploymentUrl(personal, ota) == personal);
    assert(endpoint_policy::DeploymentUrl("", ota) == ota);
    assert(endpoint_policy::DeploymentUrl("http://home.example/path", ota).empty());
    assert(endpoint_policy::DeploymentUrl("https://api.tenclass.net/path", ota).empty());
    assert(endpoint_policy::DeploymentUrl("https://child.xiaozhi.me/path", ota).empty());
    assert(endpoint_policy::DeploymentUrl("https://bad.example/#fragment", ota).empty());
    assert(endpoint_policy::DeploymentUrl("", "https://api.tenclass.net/path").empty());
    assert(endpoint_policy::AllowedWebsocket(endpoint_policy::DeploymentUrl(personal, ota), "wss://home.example:18444/ws"));
    assert(!endpoint_policy::AllowedWebsocket(endpoint_policy::DeploymentUrl(personal, ota), "wss://echo.example.com:18444/ws"));
    assert(endpoint_policy::ValidProvisionedWebsocket(personal, "wss://home.example:18444/ws", "token"));
    assert(!endpoint_policy::ValidProvisionedWebsocket(personal, "wss://home.example:18444/ws", ""));
    assert(!endpoint_policy::ValidProvisionedWebsocket(personal, "wss://home.example:18444/ws", "token\r\ninjected"));
    assert(!endpoint_policy::ValidProvisionedWebsocket(personal, "wss://other.example:18444/ws", "token"));
    // A failed initial OTA check requires a refresh even when origin matches.
    assert(endpoint_policy::NeedOtaRefresh(true, alternate, "wss://home.example:18444/ws"));
    PersistentLinkPolicy retry;
    assert(retry.Due(0)); retry.Failed(0,0); assert(!retry.Due(999999)); assert(retry.Due(1000000));
    retry.Failed(1000000,0); assert(!retry.Due(2499999)); assert(retry.Due(2500000));
    retry.Succeeded(); assert(retry.Due(0));
    assert(RecoveryClockSampleIsCurrent(true, 3, 3, 8, 8, 8));
    assert(!RecoveryClockSampleIsCurrent(false, 3, 3, 8, 8, 8));
    assert(!RecoveryClockSampleIsCurrent(true, 4, 3, 8, 8, 8));
    assert(!RecoveryClockSampleIsCurrent(true, 3, 3, 8, 9, 8));
    assert(!RecoveryClockSampleIsCurrent(true, 3, 3, 8, 8, 9));
    assert(!endpoint_policy::OldMqttEndpoint("api.tenclass.net.evil:1883"));
    assert(endpoint_policy::OldMqttEndpoint("API.TENCLASS.NET:1883"));
    const std::map<std::string,std::string> original{{"endpoint","api.tenclass.net:1883"},{"password","x"},{"username","x"},{"client_id","x"},{"publish_topic","x"},{"subscribe_topic","x"},{"unrelated","preserve"}};
    for (int failure=0; failure<6; ++failure) {
        FakeNvs store; store.data=original; store.fail_erase=failure; bool started=false;
        assert(RemoveOldMqtt(store, started)==Result::Retry);
        assert(store.data.count("endpoint") == 1); // endpoint removed only after all earlier keys
        started=false; // emulate loss of RAM on restart
        store.fail_erase=-1; store.erase_count=0;
        assert(RemoveOldMqtt(store, started)==Result::Removed);
        assert(store.data.size()==1 && store.data.at("unrelated")=="preserve");
    }
    for (int mode=0; mode<4; ++mode) {
        FakeNvs store; store.data=original; bool started=false;
        store.fail_read=mode==0; store.fail_open=mode==1; store.fail_commit=mode==2; store.fail_verify=mode==3;
        assert(RemoveOldMqtt(store, started)==Result::Retry);
        store.fail_read=store.fail_open=store.fail_commit=store.fail_verify=false;
        assert(RemoveOldMqtt(store, started)==Result::Removed);
        assert(store.data.size()==1 && store.data.at("unrelated")=="preserve");
    }
    FakeNvs other; other.data=original; other.data["endpoint"]="different.example:1883"; bool started=false;
    assert(RemoveOldMqtt(other, started)==Result::PreservedOther && other.data.size()==original.size());
    FakeNvs absent; assert(RemoveOldMqtt(absent, started)==Result::Absent);
    std::cout << "endpoint policy and legacy MQTT cleanup passed\n";
}

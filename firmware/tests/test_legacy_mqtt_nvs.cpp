#include "../main/legacy_mqtt_nvs.h"
#include <cassert>
#include <cstring>
#include <map>
#include <iostream>
static std::map<std::string,std::string> entries;
static bool exists=true, bad_open=false, bad_commit=false, bad_verify=false;
static int read_opens=0;
int nvs_open(const char* ns, int mode, nvs_handle_t* h) {
    assert(std::string(ns)=="mqtt");
    if (bad_open) return ESP_FAIL;
    if (!exists) return ESP_ERR_NVS_NOT_FOUND;
    if (mode==NVS_READONLY && ++read_opens>1 && bad_verify) return ESP_FAIL;
    *h=1; return ESP_OK;
}
int nvs_get_str(nvs_handle_t,const char* key,char* buf,size_t* len) {
    const auto it=entries.find(key); if(it==entries.end()) return ESP_ERR_NVS_NOT_FOUND;
    const auto n=it->second.size()+1; if(buf) { if(*len<n) return ESP_FAIL; std::memcpy(buf,it->second.c_str(),n); }
    *len=n; return ESP_OK;
}
int nvs_erase_key(nvs_handle_t,const char* key) { return entries.erase(key) ? ESP_OK : ESP_ERR_NVS_NOT_FOUND; }
int nvs_commit(nvs_handle_t) { return bad_commit ? ESP_FAIL : ESP_OK; }
void nvs_close(nvs_handle_t) {}
int main() {
    using namespace legacy_cleanup;
    bool started=false;
    exists=false; LegacyMqttNvs no_namespace; assert(RemoveOldMqtt(no_namespace,started)==Result::Absent);
    exists=true;
    entries={{"endpoint","api.tenclass.net:1883"},{"password","dummy"},{"unrelated","saved"}};
    LegacyMqttNvs store; read_opens=0; bad_commit=true;
    assert(RemoveOldMqtt(store,started)==Result::Retry && started);
    bad_commit=false; read_opens=0;
    assert(RemoveOldMqtt(store,started)==Result::Removed && !started);
    assert(entries.size()==1 && entries.at("unrelated")=="saved");
    entries={{"endpoint","api.tenclass.net:1883"},{"password","dummy"}};
    read_opens=0; bad_verify=true;
    assert(RemoveOldMqtt(store,started)==Result::Retry && started);
    bad_verify=false; read_opens=0;
    assert(RemoveOldMqtt(store,started)==Result::Removed && !started);
    assert(entries.empty());
    std::cout << "NVS adapter fault tests passed\n";
}

#include "alarm_store.h"
#include "nvs.h"
#include "cJSON.h"
#include <cassert>
#include <cstdlib>
#include <cstring>
#include <string>

static std::string saved;
static std::string pending;
static int commits;
static bool fail_commit;
static int allocations_until_failure;

esp_err_t nvs_open(const char*, int, nvs_handle_t* h) { *h = 1; return ESP_OK; }
esp_err_t nvs_get_str(nvs_handle_t, const char*, char* dst, size_t* n) {
    if (saved.empty()) return ESP_ERR_NVS_NOT_FOUND;
    if (!dst) { *n = saved.size() + 1; return ESP_OK; }
    if (*n < saved.size()+1) return 2;
    std::memcpy(dst, saved.c_str(), saved.size()+1); return ESP_OK;
}
esp_err_t nvs_set_str(nvs_handle_t, const char*, const char* s) { pending = s; return ESP_OK; }
esp_err_t nvs_commit(nvs_handle_t) { ++commits; if (fail_commit) return 2; saved = pending; return ESP_OK; }
void nvs_close(nvs_handle_t) {}
static void* FailAlloc(size_t n) {
    if (allocations_until_failure-- <= 0) return nullptr;
    return std::malloc(n);
}
static void SetHooks(bool fail) {
    if (!fail) { cJSON_InitHooks(nullptr); return; }
    cJSON_Hooks hooks{FailAlloc, std::free}; cJSON_InitHooks(&hooks);
}
int main() {
    bool saw_success = false;
    for (int failures = 0; failures < 80; ++failures) {
        saved.clear(); pending.clear(); commits=0; fail_commit=false;
        AlarmStore store; assert(store.Load());
        allocations_until_failure = failures; SetHooks(true);
        auto r = store.Create(1728021600, 7, 30, 0, false);
        SetHooks(false);
        if (r.error == echoalarm::Error::Ok) { assert(commits == 1); saw_success=true; }
        else { assert(r.error == echoalarm::Error::StorageFailure); assert(commits == 0); assert(saved.empty()); }
    }
    assert(saw_success);
    saved.clear(); pending.clear(); commits=0; fail_commit=true;
    AlarmStore store; assert(store.Load());
    auto failure=store.Create(1728021600,7,30,0,false);
    assert(failure.error == echoalarm::Error::StorageFailure && saved.empty());
    assert(store.Remove(1).error == echoalarm::Error::NotFound);
    fail_commit=false;
    auto success=store.Create(1728021600,7,30,0,false);
    assert(success.error == echoalarm::Error::Ok && !saved.empty());
    AlarmStore restored; assert(restored.Load());
    assert(restored.Remove(success.id).error == echoalarm::Error::Ok);
    saved.clear(); pending.clear(); commits=0; fail_commit=true;
    AlarmStore relative; assert(relative.Load());
    auto rfail=relative.CreateAfter(1728021600,1,2);
    assert(rfail.error==echoalarm::Error::StorageFailure && saved.empty());
    assert(relative.Remove(1).error==echoalarm::Error::NotFound);
    fail_commit=false;
    auto rok=relative.CreateAfter(1728021600,1,2);
    assert(rok.error==echoalarm::Error::Ok && rok.state.alarms[0].next==1728021780);
    AlarmStore reloaded; assert(reloaded.Load());
    assert(reloaded.Remove(rok.id).error==echoalarm::Error::Ok);
    AlarmStore uninitialized;
    assert(uninitialized.CreateAfter(1728021600,1,2).error==echoalarm::Error::StorageFailure);
}

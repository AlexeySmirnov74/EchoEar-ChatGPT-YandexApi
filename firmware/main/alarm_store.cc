#include "alarm_store.h"
#include <nvs.h>
#include <cJSON.h>
#include <esp_log.h>
#include <cstdlib>
#include <cmath>
#include <set>

namespace {
constexpr const char* kNamespace = "echo_alarms";
constexpr const char* kKey = "state_v1";
constexpr const char* kTag = "AlarmStore";
constexpr int64_t kMaxEpoch = 4102444800LL; // 2100-01-01
bool Integer(const cJSON* v, int64_t min, int64_t max) {
    return cJSON_IsNumber(v) && std::isfinite(v->valuedouble) &&
           std::trunc(v->valuedouble) == v->valuedouble &&
           v->valuedouble >= min && v->valuedouble <= max;
}
}

bool AlarmStore::Save(const echoalarm::State& s) {
    cJSON* root = cJSON_CreateObject();
    if (!root) return false;
    if (!cJSON_AddNumberToObject(root, "version", 1) || !cJSON_AddNumberToObject(root, "next_id", s.next_id)) {
        cJSON_Delete(root); return false;
    }
    cJSON* alarms = cJSON_AddArrayToObject(root, "alarms");
    if (!alarms) { cJSON_Delete(root); return false; }
    for (const auto& alarm : s.alarms) if (alarm.id) {
        auto* item = cJSON_CreateObject();
        if (!item) { cJSON_Delete(root); return false; }
        if (!cJSON_AddNumberToObject(item, "id", alarm.id) ||
            !cJSON_AddNumberToObject(item, "next", static_cast<double>(alarm.next)) ||
            !cJSON_AddBoolToObject(item, "daily", alarm.mode == echoalarm::Mode::Daily) ||
            !cJSON_AddBoolToObject(item, "ringing", alarm.ringing) ||
            !cJSON_AddNumberToObject(item, "ring_started", static_cast<double>(alarm.ring_started)) ||
            !cJSON_AddItemToArray(alarms, item)) { cJSON_Delete(item); cJSON_Delete(root); return false; }
    }
    char* json = cJSON_PrintUnformatted(root);
    cJSON_Delete(root);
    if (!json) return false;
    nvs_handle_t handle = 0;
    esp_err_t err = nvs_open(kNamespace, NVS_READWRITE, &handle);
    if (err == ESP_OK) {
        err = nvs_set_str(handle, kKey, json);
        if (err == ESP_OK) err = nvs_commit(handle);
        nvs_close(handle);
    }
    cJSON_free(json);
    if (err != ESP_OK) ESP_LOGE(kTag, "NVS save failed: %s", esp_err_to_name(err));
    return err == ESP_OK;
}

bool AlarmStore::Load() {
    nvs_handle_t handle = 0;
    esp_err_t err = nvs_open(kNamespace, NVS_READWRITE, &handle);
    if (err != ESP_OK) return ready_ = false;
    size_t size = 0;
    err = nvs_get_str(handle, kKey, nullptr, &size);
    if (err == ESP_ERR_NVS_NOT_FOUND) { nvs_close(handle); state_ = {}; return ready_ = true; }
    if (err != ESP_OK || size == 0 || size > 4096) { nvs_close(handle); return ready_ = false; }
    std::string data(size, '\0');
    err = nvs_get_str(handle, kKey, data.data(), &size);
    nvs_close(handle);
    if (err != ESP_OK) return ready_ = false;
    cJSON* root = cJSON_Parse(data.c_str());
    if (!root) return ready_ = false;
    auto* version = cJSON_GetObjectItemCaseSensitive(root, "version");
    auto* next_id = cJSON_GetObjectItemCaseSensitive(root, "next_id");
    auto* alarms = cJSON_GetObjectItemCaseSensitive(root, "alarms");
    bool ok = Integer(version, 1, 1) && Integer(next_id, 0, UINT32_MAX) && cJSON_IsArray(alarms) &&
              cJSON_GetArraySize(alarms) <= echoalarm::kMaximum;
    echoalarm::State next{};
    std::set<uint32_t> ids;
    if (ok) {
        next.next_id = static_cast<uint32_t>(next_id->valuedouble);
        int index = 0;
        cJSON* item = nullptr;
        cJSON_ArrayForEach(item, alarms) {
            auto* id = cJSON_GetObjectItemCaseSensitive(item, "id");
            auto* when = cJSON_GetObjectItemCaseSensitive(item, "next");
            auto* daily = cJSON_GetObjectItemCaseSensitive(item, "daily");
            auto* ringing = cJSON_GetObjectItemCaseSensitive(item, "ringing");
            auto* started = cJSON_GetObjectItemCaseSensitive(item, "ring_started");
            if (!Integer(id, 1, echoalarm::kMaxId) || !Integer(when, echoalarm::kMinTrustedLocalEpoch, kMaxEpoch) ||
                !cJSON_IsBool(daily) || !cJSON_IsBool(ringing) || !Integer(started, 0, kMaxEpoch) ||
                !ids.insert(static_cast<uint32_t>(id->valuedouble)).second ||
                (cJSON_IsTrue(ringing) && started->valuedouble < echoalarm::kMinTrustedLocalEpoch)) { ok = false; break; }
            next.alarms[index++] = {static_cast<uint32_t>(id->valuedouble), static_cast<int64_t>(when->valuedouble),
                cJSON_IsTrue(daily) ? echoalarm::Mode::Daily : echoalarm::Mode::Once, cJSON_IsTrue(ringing) != 0,
                static_cast<int64_t>(started->valuedouble)};
        }
    }
    cJSON_Delete(root);
    if (!ok) { ESP_LOGE(kTag, "Stored alarm data invalid; refusing to overwrite"); return ready_ = false; }
    state_ = next;
    return ready_ = true;
}

echoalarm::Result AlarmStore::Create(int64_t now, int hour, int minute, int day, bool daily) {
    if (!ready_) return {state_, echoalarm::Error::StorageFailure, 0};
    auto result = echoalarm::Create(state_, now, hour, minute, day, daily ? echoalarm::Mode::Daily : echoalarm::Mode::Once);
    if (result.error == echoalarm::Error::Ok) {
        if (!Save(result.state)) return {state_, echoalarm::Error::StorageFailure, 0};
        state_ = result.state;
    }
    return result;
}
echoalarm::Result AlarmStore::CreateAfter(int64_t now, int microseconds, int minutes) {
    if (!ready_) return {state_, echoalarm::Error::StorageFailure, 0};
    auto result = echoalarm::CreateAfter(state_, now, microseconds, minutes);
    if (result.error == echoalarm::Error::Ok) {
        if (!Save(result.state)) return {state_, echoalarm::Error::StorageFailure, 0};
        state_ = result.state;
    }
    return result;
}
echoalarm::Result AlarmStore::Remove(uint32_t id) {
    if (!ready_) return {state_, echoalarm::Error::StorageFailure, 0};
    auto result = echoalarm::Remove(state_, id);
    if (result.error == echoalarm::Error::Ok) {
        if (!Save(result.state)) return {state_, echoalarm::Error::StorageFailure, 0};
        state_ = result.state;
    }
    return result;
}
bool AlarmStore::Dismiss(int64_t now, uint32_t id) {
    if (!ready_) return false;
    auto result = echoalarm::Dismiss(state_, now, id);
    if (result.error != echoalarm::Error::Ok || !Save(result.state)) return false;
    state_ = result.state;
    return true;
}
echoalarm::TickResult AlarmStore::Tick(int64_t now) {
    if (!ready_) return {state_, false, false, 0};
    auto result = echoalarm::Tick(state_, now);
    if (result.changed && !Save(result.state)) { ESP_LOGE("AlarmDiag", "tick_save_failed"); return {state_, false, false, 0}; }
    if (result.changed) state_ = result.state;
    return result;
}
std::string AlarmStore::Describe() const {
    if (!ready_) return "{\"ok\":false,\"error\":\"storage_unavailable\"}";
    cJSON* root = cJSON_CreateObject();
    if (!root) return "{\"ok\":false}";
    if (!cJSON_AddBoolToObject(root, "ok", true)) { cJSON_Delete(root); return "{\"ok\":false}"; }
    cJSON* list = cJSON_AddArrayToObject(root, "alarms");
    if (!list) { cJSON_Delete(root); return "{\"ok\":false}"; }
    for (const auto& a : state_.alarms) if (a.id) {
        cJSON* item = cJSON_CreateObject();
        if (!item) { cJSON_Delete(root); return "{\"ok\":false}"; }
        if (!cJSON_AddNumberToObject(item, "id", a.id) ||
            !cJSON_AddNumberToObject(item, "local_epoch", static_cast<double>(a.next)) ||
            !cJSON_AddBoolToObject(item, "daily", a.mode == echoalarm::Mode::Daily) ||
            !cJSON_AddBoolToObject(item, "ringing", a.ringing) ||
            !cJSON_AddItemToArray(list, item)) { cJSON_Delete(item); cJSON_Delete(root); return "{\"ok\":false}"; }
    }
    char* json = cJSON_PrintUnformatted(root);
    std::string output = json ? json : "{\"ok\":false}";
    cJSON_free(json);
    cJSON_Delete(root);
    return output;
}

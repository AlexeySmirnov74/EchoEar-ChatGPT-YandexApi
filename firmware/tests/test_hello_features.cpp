#include "../main/protocols/hello_features.h"
#include <cassert>
#include <string>

static bool ContainsPersistentFeature(const std::atomic_bool* enabled) {
    cJSON* features = cJSON_CreateObject();
    assert(features);
    AddPersistentIdleHelloFeature(features, enabled);
    const cJSON* capability = cJSON_GetObjectItemCaseSensitive(features, "persistent_idle");
    if (capability) assert(cJSON_IsTrue(capability));
    char* serialized = cJSON_PrintUnformatted(features);
    assert(serialized);
    const std::string json(serialized);
    cJSON_free(serialized);
    cJSON_Delete(features);
    return json.find("\"persistent_idle\":true") != std::string::npos;
}

int main() {
    std::atomic_bool enabled{false};
    assert(!ContainsPersistentFeature(nullptr));
    assert(!ContainsPersistentFeature(&enabled)); // event registration failure
    enabled.store(true);
    assert(ContainsPersistentFeature(&enabled)); // negotiated startup
    enabled.store(false);
    assert(!ContainsPersistentFeature(&enabled)); // legacy fallback
}

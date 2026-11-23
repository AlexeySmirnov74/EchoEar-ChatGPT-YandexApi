#pragma once

#include <atomic>
#include <cJSON.h>

// The source flag belongs to Application and outlives the protocol. Omit the
// capability entirely in legacy mode, including failed event registration.
inline void AddPersistentIdleHelloFeature(cJSON* features, const std::atomic_bool* enabled) {
    if (enabled && enabled->load()) cJSON_AddBoolToObject(features, "persistent_idle", true);
}

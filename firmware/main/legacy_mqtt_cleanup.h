#pragma once
#include "endpoint_policy.h"
#include <string>

namespace legacy_cleanup {
enum class Result { Removed, Absent, PreservedOther, Retry };
enum class Read { Found, Missing, Error };
// Endpoint last: after interruption, its presence causes safe idempotent retry.
template<class Store> Result RemoveOldMqtt(Store& store, bool& started) {
    std::string endpoint;
    const auto read = store.ReadEndpoint(endpoint);
    if (read == Read::Error) return Result::Retry;
    if (read == Read::Missing && !started) return store.Verify() ? Result::Absent : Result::Retry;
    if (read == Read::Found && !endpoint_policy::OldMqttEndpoint(endpoint)) return Result::PreservedOther;
    if (!store.Begin()) return Result::Retry;
    started = true;
    for (const char* key : {"password", "username", "client_id", "publish_topic", "subscribe_topic", "endpoint"}) {
        if (!store.Erase(key)) { store.End(); return Result::Retry; }
    }
    const bool committed = store.Commit();
    store.End();
    if (!committed || !store.Verify()) return Result::Retry;
    started = false;
    return Result::Removed;
}
}

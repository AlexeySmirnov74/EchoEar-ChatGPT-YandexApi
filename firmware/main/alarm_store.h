#pragma once
#include "alarm_scheduler.h"
#include <cstdint>
#include <string>

// All methods are invoked from Application's main loop, including MCP calls.
// Mutations are visible to callers only after NVS confirms the commit.
class AlarmStore {
public:
    bool Load();
    echoalarm::Result Create(int64_t local_now, int hour, int minute, int day, bool daily);
    echoalarm::Result CreateAfter(int64_t local_now, int microseconds, int minutes);
    echoalarm::Result Remove(uint32_t id);
    bool Dismiss(int64_t local_now, uint32_t id);
    echoalarm::TickResult Tick(int64_t local_now);
    std::string Describe() const;
private:
    bool Save(const echoalarm::State& state);
    echoalarm::State state_{};
    bool ready_ = false;
};

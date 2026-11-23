#pragma once

// Pure, bounded alarm scheduling policy. The caller persists the returned
// state atomically before acknowledging a change or starting the buzzer.
// This module deliberately knows nothing about time zones or network time.
#include <array>
#include <cstdint>

namespace echoalarm {
constexpr int kMaximum = 8;
constexpr uint32_t kMaxId = 2147483647U;
constexpr int64_t kDay = 86400;
constexpr int64_t kMaxLateness = 300;
constexpr int64_t kMinTrustedLocalEpoch = 1700000000;
constexpr int64_t kMaxTrustedLocalEpoch = 4102444800LL;

enum class Mode : uint8_t { Once = 0, Daily = 1 };
struct Alarm {
    uint32_t id = 0;
    int64_t next = 0;
    Mode mode = Mode::Once;
    bool ringing = false;
    int64_t ring_started = 0;
};
struct State {
    uint32_t next_id = 1;
    std::array<Alarm, kMaximum> alarms{};
};
enum class Error { Ok, InvalidClock, InvalidArgument, Full, NotFound, StorageFailure };
struct Result { State state; Error error; uint32_t id; };
struct TickResult { State state; bool changed; bool ring; uint32_t id; };

inline bool ValidClock(int64_t now) { return now >= kMinTrustedLocalEpoch; }

// day=0 means next occurrence; day=1 means tomorrow; day=2 means today.
// Inputs use the device's existing local-time epoch contract. The caller must
// independently verify that the device clock has been synchronised this boot.
inline Result Create(const State& old, int64_t now, int hour, int minute, int day, Mode mode) {
    if (!ValidClock(now)) return {old, Error::InvalidClock, 0};
    if (hour < 0 || hour > 23 || minute < 0 || minute > 59 || day < 0 || day > 2 ||
        (mode != Mode::Once && mode != Mode::Daily)) return {old, Error::InvalidArgument, 0};
    auto s = old;
    for (auto& a : s.alarms) {
        if (a.id != 0) continue;
        int64_t next = (now / kDay) * kDay + hour * 3600 + minute * 60;
        if (day == 1 || (day == 0 && next <= now)) next += kDay;
        if (next <= now) return {old, Error::InvalidArgument, 0};
        uint32_t id = s.next_id;
        // Avoid reusing a live id after wrap-around or corrupt data.
        for (int attempt = 0; attempt <= kMaximum; ++attempt) {
            if (id == 0 || id > kMaxId) id = 1;
            bool occupied = false;
            for (const auto& existing : s.alarms) if (existing.id == id) occupied = true;
            if (!occupied) break;
            ++id;
        }
        a = {id, next, mode, false, 0};
        s.next_id = id + 1;
        return {s, Error::Ok, id};
    }
    return {old, Error::Full, 0};
}

// Relative requests use the device clock. Round toward the next whole minute
// so the alarm never fires before the requested duration (including a
// fractional current second). An alarm can consequently be up to 60s later.
inline Result CreateAfter(const State& old, int64_t now, int microseconds, int minutes) {
    if (!ValidClock(now) || now >= kMaxTrustedLocalEpoch) return {old, Error::InvalidClock, 0};
    if (minutes < 1 || minutes > 1439 || microseconds < 0 || microseconds >= 1000000)
        return {old, Error::InvalidArgument, 0};
    const int64_t remainder = now % 60;
    const int64_t rounded = now + ((remainder != 0 || microseconds != 0) ? 60 - remainder : 0);
    const int64_t target = rounded + static_cast<int64_t>(minutes) * 60;
    if (target > kMaxTrustedLocalEpoch) return {old, Error::InvalidClock, 0};
    const int day = target / kDay == now / kDay ? 2 : 1;
    const int64_t seconds_of_day = target % kDay;
    return Create(old, now, static_cast<int>(seconds_of_day / 3600),
                  static_cast<int>((seconds_of_day % 3600) / 60), day, Mode::Once);
}

inline Result Remove(const State& old, uint32_t id) {
    if (!id) return {old, Error::InvalidArgument, 0};
    auto s = old;
    for (auto& a : s.alarms) if (a.id == id) {
        a = Alarm{};
        return {s, Error::Ok, id};
    }
    return {old, Error::NotFound, 0};
}

inline void Advance(Alarm& alarm, int64_t now) {
    if (alarm.mode == Mode::Once) { alarm = Alarm{}; return; }
    // Advance from the scheduled local wall time rather than from completion,
    // to avoid drift and prevent repeated firing after clock correction.
    do { alarm.next += kDay; } while (alarm.next <= now);
    alarm.ringing = false;
    alarm.ring_started = 0;
}

inline Result Dismiss(const State& old, int64_t now, uint32_t id) {
    auto s = old;
    for (auto& a : s.alarms) if (a.id == id && a.ringing) {
        Advance(a, now);
        return {s, Error::Ok, id};
    }
    return {old, Error::NotFound, 0};
}

// A persisted active ring continues until explicitly dismissed or removed.
// It survives reboot without waiting for clock synchronization, and backwards clock changes.
// A pending alarm still expires when first observed over five minutes late.
inline TickResult Tick(const State& old, int64_t now) {
    auto s = old;
    bool changed = false;
    for (auto& a : s.alarms) if (a.id && a.ringing) {
        return {s, false, true, a.id};
    }
    if (!ValidClock(now)) return {old, false, false, 0};
    for (auto& a : s.alarms) if (a.id && !a.ringing && a.next <= now) {
        if (now - a.next > kMaxLateness) { Advance(a, now); changed = true; continue; }
        a.ringing = true;
        // Record activation once; periodic ticks do not write to NVS.
        a.ring_started = now;
        changed = true;
        return {s, true, true, a.id};
    }
    return {s, changed, false, 0};
}
}  // namespace echoalarm

#pragma once
#include <cstdint>

namespace echorest {
// Deadlines use monotonic microseconds. Close never means power off.
constexpr int64_t kNoSpeechWaitUs = 15'000'000;
constexpr int64_t kStreamHardLimitUs = 90'000'000;
constexpr int64_t kDrainSettleUs = 250'000;
struct CloseInput {
    bool pending, retired, alarm, disconnected, tts_started, tts_finished, drained_stable;
    int64_t elapsed_us;
};
inline bool ShouldClose(const CloseInput& v) {
    if (!v.pending || v.retired) return false;
    if (v.alarm || v.disconnected || v.elapsed_us >= kStreamHardLimitUs) return true;
    return v.drained_stable && (v.tts_finished || (!v.tts_started && v.elapsed_us >= kNoSpeechWaitUs));
}
inline bool WakeReady(bool prompt_feeding, bool playback_drained_stable, bool connected, bool alarm) {
    return !prompt_feeding && playback_drained_stable && connected && !alarm;
}
} // namespace echorest

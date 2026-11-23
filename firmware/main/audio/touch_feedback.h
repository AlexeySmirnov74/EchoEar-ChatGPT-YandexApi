#pragma once

#include <algorithm>
#include <atomic>
#include <cstddef>
#include <cstdint>

// A short, envelope-shaped 1200-Hz confirmation mixed into existing PCM.
// Producers can request without waiting; only the output task calls Mix().
class TouchFeedback {
public:
    static constexpr uint32_t kMaxPending = 8;
    static constexpr int kDurationMs = 72;
    static constexpr uint32_t kMicMaskMs = 120;

    bool Request() {
        uint32_t n = state_.load();
        do {
            if ((n & kPendingMask) >= kMaxPending) return false;
        } while (!state_.compare_exchange_weak(n, n + 1));
        return true;
    }
    bool HasWork() const { return state_.load() != 0; }
    bool HasOutstanding() const { return state_.load() != 0; }
    // Atomically remove queued cues. False means an output block has already
    // begun; the caller must not announce listening until it has drained.
    bool CancelPending() { return (state_.fetch_and(kActive) & kActive) == 0; }
    uint32_t SamplesRemaining(int sample_rate) const {
        return active_ ? total_ - cursor_ : static_cast<uint32_t>(sample_rate) * kDurationMs / 1000;
    }

    // Returns true only when this block begins a cue. A cue persists across
    // playback blocks, including unrelated decoder resets.
    bool Mix(int16_t* pcm, size_t count, int sample_rate) {
        if (sample_rate <= 0 || !pcm || count == 0) return false;
        bool started = false;
        if (!active_) {
            uint32_t n = state_.load();
            do {
                if (!(n & kPendingMask)) return false;
            } while (!state_.compare_exchange_weak(n, (n - 1) | kActive));
            cursor_ = 0;
            total_ = static_cast<uint32_t>(sample_rate) * kDurationMs / 1000;
            active_ = true;
            started = true;
        }
        static constexpr int16_t sine[] = {
            0, 618, 1176, 1618, 1902, 2000, 1902, 1618, 1176, 618,
            0, -618, -1176, -1618, -1902, -2000, -1902, -1618, -1176, -618
        };
        const uint32_t fade = std::max(1, sample_rate / 250); // 4 ms
        for (size_t i = 0; i < count && cursor_ < total_; ++i, ++cursor_) {
            uint32_t env = std::min({fade, cursor_, total_ - 1 - cursor_});
            auto phase = (static_cast<uint64_t>(cursor_) * 1200 * 20 / sample_rate) % 20;
            int value = pcm[i] + sine[phase] * static_cast<int>(env) / static_cast<int>(fade);
            pcm[i] = static_cast<int16_t>(std::max(-32768, std::min(32767, value)));
        }
        if (cursor_ >= total_) complete_ = true;
        return started;
    }
    // The active bit stays set while the final block is in the codec write.
    // A timeout can therefore never declare readiness while that write stalls.
    void FinishOutput() {
        if (complete_) {
            complete_ = false;
            active_ = false;
            state_.fetch_and(kPendingMask);
        }
    }
private:
    static constexpr uint32_t kPendingMask = 0xff;
    static constexpr uint32_t kActive = 0x100;
    std::atomic<uint32_t> state_{0};
    // Only the audio output task reads/writes these fields, except HasWork,
    // which is called exclusively by that same task.
    bool active_ = false, complete_ = false;
    uint32_t cursor_ = 0, total_ = 0;
};

inline bool TouchFeedbackMuteCapture(uint32_t capture_start_ms, uint32_t mute_until_ms) {
    return mute_until_ms && static_cast<int32_t>(mute_until_ms - capture_start_ms) > 0;
}

// Estimate sample timestamps backwards from the completed read, then mute
// only the portion overlapping the cue's echo window. Preserve later speech
// even when it shares an input block with the final muted samples. For stereo
// input, keep the reference channel intact for echo cancellation.
inline void TouchFeedbackMaskInput(int16_t* samples, size_t count, int channels,
                                   int sample_rate, uint32_t capture_end_ms,
                                   uint32_t mute_until_ms) {
    if (!samples || channels < 1 || sample_rate <= 0 || !mute_until_ms) return;
    const int64_t frames = count / channels;
    const int64_t duration_us = frames * 1000000 / sample_rate;
    const int64_t end_us = static_cast<int32_t>(mute_until_ms - capture_end_ms) * 1000LL + duration_us;
    const int64_t start_us = end_us - TouchFeedback::kMicMaskMs * 1000LL;
    auto index = [frames, sample_rate](int64_t offset_us) {
        if (offset_us <= 0) return int64_t{0};
        return std::min<int64_t>(frames, (offset_us * sample_rate + 999999) / 1000000);
    };
    const int64_t first = index(start_us), last = index(end_us);
    for (int64_t i = first; i < last; ++i) samples[i * channels] = 0;
}

#pragma once

#include <cstdint>

inline bool PersistentEventIsCurrent(bool negotiated, uint32_t active, uint32_t incoming) {
    return !negotiated || (incoming != 0 && incoming == active);
}

inline bool PersistentStandbyIsCurrent(bool negotiated, uint32_t active, uint32_t incoming, bool listening) {
    return negotiated && listening && PersistentEventIsCurrent(true, active, incoming);
}

inline bool PersistentCallbackIsCurrent(bool enabled, uint32_t active_epoch, uint32_t callback_epoch) {
    return enabled && active_epoch == callback_epoch;
}

// Entering listening after an answer begins a new protocol generation even
// when Realtime/AEC kept the local voice processor running during speech.
inline bool PrepareListeningStart(bool negotiated, bool processor_running, uint32_t& listen_id) {
    if (!negotiated && processor_running) return false;
    if (negotiated && ++listen_id == 0) ++listen_id;
    return true;
}

inline bool NetworkGenerationChanged(uint32_t observed, uint32_t handled) {
    return observed != handled;
}

inline bool NetworkAttemptIsCurrent(uint32_t attempt, uint32_t observed, uint32_t handled) {
    return attempt == observed && attempt == handled;
}
inline bool RecoveryClockSampleIsCurrent(bool enabled, uint32_t active_epoch, uint32_t sample_epoch,
                                         uint32_t sample_network, uint32_t observed_network, uint32_t handled_network) {
    return PersistentCallbackIsCurrent(enabled, active_epoch, sample_epoch) &&
           NetworkAttemptIsCurrent(sample_network, observed_network, handled_network);
}

// Main event-loop owned. Network attempts run asynchronously and call back
// into the event loop; this policy never performs I/O itself.
class PersistentLinkPolicy {
public:
    bool Due(int64_t now_us) const { return now_us >= next_attempt_us_; }
    void Succeeded() { delay_seconds_ = 1; next_attempt_us_ = 0; }
    void Failed(int64_t now_us, uint32_t random_value) {
        const int64_t upper_us = static_cast<int64_t>(delay_seconds_) * 1000000;
        const int64_t lower_us = upper_us > 1000000 ? upper_us * 3 / 4 : 1000000;
        const auto range_us = static_cast<uint32_t>(upper_us - lower_us);
        next_attempt_us_ = now_us + lower_us + random_value % (range_us + 1);
        delay_seconds_ = delay_seconds_ < 16 ? delay_seconds_ * 2 : 30;
    }
    void RetryNow() { next_attempt_us_ = 0; }

private:
    int64_t next_attempt_us_ = 0;
    int delay_seconds_ = 1;
};

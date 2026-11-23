#pragma once

namespace echowake {
enum class Phase { First, AwaitReady, Ready };
enum class Action { Wait, RestartFirst, FirstFinished, StartReady, CancelReady, Done };

struct Input {
    Phase phase;
    bool alarm;
    bool connected;
    bool connection_busy;
    bool same_audio_generation;
    bool same_connection_generation;
    bool feeding;
    bool drained_stable;
    bool succeeded;
};

inline Action Decide(const Input& x) {
    if (x.alarm) return Action::Wait;
    switch (x.phase) {
    case Phase::First:
        if (!x.same_audio_generation)
            return !x.feeding && x.drained_stable ? Action::RestartFirst : Action::Wait;
        if (!x.feeding && x.drained_stable)
            return x.succeeded ? Action::FirstFinished : Action::RestartFirst;
        return Action::Wait;
    case Phase::AwaitReady:
        return x.connected && !x.feeding && x.drained_stable ? Action::StartReady : Action::Wait;
    case Phase::Ready:
        if (!x.same_connection_generation || !x.same_audio_generation)
            return Action::CancelReady;
        if (!x.connected) return x.connection_busy ? Action::Wait : Action::CancelReady;
        if (!x.feeding && x.drained_stable)
            return x.succeeded ? Action::Done : Action::CancelReady;
        return Action::Wait;
    }
    return Action::Wait;
}
}

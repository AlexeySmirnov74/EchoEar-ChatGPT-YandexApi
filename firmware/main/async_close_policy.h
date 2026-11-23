#pragma once

#include <atomic>
#include <cstdint>

// Begin/Poll run on the event loop. Complete runs after the worker has stopped
// touching Protocol. A timeout never transfers ownership away from the worker.
class AsyncClosePolicy {
public:
    enum class Result { Idle, Waiting, Complete, TimedOut };

    bool Begin(int64_t now_us) {
        if (state_.load() != State::Idle) return false;
        deadline_us_ = now_us + 20'000'000;
        state_.store(State::Running);
        return true;
    }

    void Complete() { state_.store(State::Done); }

    // Only use after task creation failed: no worker owns the protocol.
    void CancelUnstarted() { state_.store(State::Idle); }

    Result Poll(int64_t now_us) {
        const auto state = state_.load();
        if (state == State::Idle) return Result::Idle;
        if (state == State::Done) {
            state_.store(State::Idle);
            return Result::Complete;
        }
        return now_us >= deadline_us_ ? Result::TimedOut : Result::Waiting;
    }

private:
    enum class State { Idle, Running, Done };
    std::atomic<State> state_{State::Idle};
    int64_t deadline_us_ = 0;
};

#pragma once

enum class AudioChannelState { Open, Closed, Busy };

// An unavailable mutex says nothing about the connection. Never replace a
// socket until a successful probe confirms it is closed.
inline bool AudioChannelNeedsReconnect(AudioChannelState state) {
    return state == AudioChannelState::Closed;
}

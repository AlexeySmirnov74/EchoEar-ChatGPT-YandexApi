#pragma once
#include "battery_estimate.h"

namespace echoear_battery_lab {
template <class Reader>
bool Sample(Reader read, int& mean, int& percent, int& successful) {
    constexpr int kAttempts = 16;
    constexpr int kMinimumSamples = 12;
    int sum = 0;
    successful = 0;
    for (int i = 0; i < kAttempts; ++i) {
        int raw = 0;
        if (read(raw) && raw > 0 && raw < 4095) {
            sum += raw;
            ++successful;
        }
    }
    if (successful < kMinimumSamples) return false;
    const int candidate = sum / successful;
    int estimate = 0;
    if (!EstimatePercent(candidate, estimate)) return false;
    mean = candidate;
    percent = estimate;
    return true;
}
}

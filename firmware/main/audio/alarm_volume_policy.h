#pragma once
// The alarm override represents a minimum, while the saved normal volume may
// change concurrently. Both numbers are read under the codec's own mutex.
inline int EffectiveAlarmVolume(int current, int floor) {
    return floor >= 0 && floor > current ? floor : current;
}

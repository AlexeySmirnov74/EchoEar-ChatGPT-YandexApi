#include "../main/rest_policy.h"
#include <cassert>
#include <cstdio>
using namespace echorest;

int main() {
    CloseInput v{true,false,false,false,false,false,true,0};
    assert(!ShouldClose(v));
    v.elapsed_us = kNoSpeechWaitUs - 1; assert(!ShouldClose(v));
    v.elapsed_us = kNoSpeechWaitUs; assert(ShouldClose(v));
    v.drained_stable = false; assert(!ShouldClose(v));
    v.tts_started = true; v.drained_stable = true; assert(!ShouldClose(v));
    v.tts_finished = true; assert(ShouldClose(v));
    v.tts_finished = false; v.elapsed_us = kStreamHardLimitUs - 1; assert(!ShouldClose(v));
    v.elapsed_us = kStreamHardLimitUs; assert(ShouldClose(v));
    v.elapsed_us = 0; v.drained_stable = false; v.disconnected = true; assert(ShouldClose(v));
    v.disconnected = false; v.alarm = true; assert(ShouldClose(v));
    v.retired = true; assert(!ShouldClose(v));
    v.retired = false; v.pending = false; assert(!ShouldClose(v));
    assert(!WakeReady(true,true,true,false));
    assert(!WakeReady(false,false,true,false));
    assert(!WakeReady(false,true,false,false));
    assert(!WakeReady(false,true,true,true));
    assert(WakeReady(false,true,true,false));
    std::puts("rest close and reconnect policy passed");
}

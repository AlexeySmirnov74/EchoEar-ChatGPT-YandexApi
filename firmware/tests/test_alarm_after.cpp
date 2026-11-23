#include "alarm_scheduler.h"
#include <cassert>
#include <cstdint>
#include <limits>
using namespace echoalarm;

int main() {
    State empty;
    constexpr int64_t midnight=20000*kDay;
    auto unsynced=CreateAfter(empty,0,0,1);
    assert(unsynced.error==Error::InvalidClock);
    assert(CreateAfter(empty,midnight,0,0).error==Error::InvalidArgument);
    assert(CreateAfter(empty,midnight,0,1440).error==Error::InvalidArgument);
    assert(CreateAfter(empty,midnight,-1,1).error==Error::InvalidArgument);
    assert(CreateAfter(empty,midnight,1000000,1).error==Error::InvalidArgument);
    assert(CreateAfter(empty,std::numeric_limits<int64_t>::max(),0,1).error==Error::InvalidClock);
    auto exact=CreateAfter(empty,midnight+12*3600,0,2);
    assert(exact.error==Error::Ok && exact.state.alarms[0].next==midnight+12*3600+120);
    auto fractional=CreateAfter(empty,midnight+12*3600,1,2);
    assert(fractional.error==Error::Ok && fractional.state.alarms[0].next==midnight+12*3600+180);
    auto endsec=CreateAfter(empty,midnight+23*3600+59*60+59,999999,1);
    assert(endsec.error==Error::Ok && endsec.state.alarms[0].next==midnight+kDay+60);
    auto max=CreateAfter(empty,midnight+23*3600+59*60+59,999999,1439);
    assert(max.error==Error::Ok && max.state.alarms[0].next==midnight+2*kDay-60);
    assert(max.state.alarms[0].mode==Mode::Once);
    assert(CreateAfter(empty,kMaxTrustedLocalEpoch-1,1,1).error==Error::InvalidClock);
    State full;
    for(int i=0;i<kMaximum;++i){auto r=CreateAfter(full,midnight+i,0,1);assert(r.error==Error::Ok);full=r.state;}
    assert(CreateAfter(full,midnight,0,1).error==Error::Full);
    // Verify no rounding sends a deadline before the requested interval.
    for(int s=0;s<86400;s+=59){for(int f: {0,1,999999}){auto r=CreateAfter(empty,midnight+s,f,1);assert(r.error==Error::Ok);const auto next=r.state.alarms[0].next;assert(next>=midnight+s+60+(f!=0));assert(next<=midnight+s+120);}}
}

#pragma once

// Experimental reproduction of the factory firmware's ADC-to-percent table.
// Not calibrated or validated as actual state of charge on this unit.
namespace echoear_battery_lab {
struct Point { int raw; int percent; };
constexpr Point kFactoryTable[] = {
    {1985, 0}, {2048, 20}, {2172, 40}, {2296, 60}, {2420, 80}, {2544, 100},
};

// false means a failed or saturated conversion, never a valid 0% reading.
inline bool EstimatePercent(int raw, int& percent) {
    if (raw <= 0 || raw >= 4095) return false;
    if (raw <= kFactoryTable[0].raw) { percent = 0; return true; }
    for (unsigned i = 1; i < sizeof(kFactoryTable) / sizeof(kFactoryTable[0]); ++i) {
        if (raw <= kFactoryTable[i].raw) {
            const auto& lo = kFactoryTable[i - 1];
            const auto& hi = kFactoryTable[i];
            percent = lo.percent + (raw - lo.raw) * (hi.percent - lo.percent) / (hi.raw - lo.raw);
            return true;
        }
    }
    percent = 100;
    return true;
}
}  // namespace echoear_battery_lab

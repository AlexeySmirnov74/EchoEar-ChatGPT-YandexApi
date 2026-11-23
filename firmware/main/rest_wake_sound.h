#pragma once
#include "assets/lang_config.h"
#include <string_view>

// Bundled 24-kHz mono Opus (60-ms packets), available without a network.
inline std::string_view GetEchoEarRestWakeSound() {
    return Lang::Sounds::OGG_REST_WAKE;
}

#pragma once
#include <cstddef>
#include <cstdint>

constexpr uint16_t WebSocketLength16(char high, char low) {
    return (static_cast<uint16_t>(static_cast<uint8_t>(high)) << 8) |
           static_cast<uint8_t>(low);
}

constexpr bool WebSocketMessageFits(size_t current, uint64_t next) {
    return current <= 65536 && next <= 65536 - current;
}

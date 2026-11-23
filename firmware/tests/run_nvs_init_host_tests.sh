#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
# Copy only to make main.cc's quoted includes resolve to the fakes; the code
# under test is the real production entry point, not a duplicate policy.
cp "$ROOT/main/main.cc" "$OUT/main.cc"
for board in 0 1; do
    g++ -std=c++17 -Wall -Wextra -Werror \
        -DCONFIG_BOARD_TYPE_ECHOEAR_2ST="$board" -DCONFIG_LOG_DYNAMIC_LEVEL_CONTROL=1 \
        -I"$ROOT/tests/nvs_init_fake" "$OUT/main.cc" "$ROOT/tests/test_nvs_init_startup.cpp" \
        -o "$OUT/nvs-$board"
    "$OUT/nvs-$board"
done
echo 'NVS startup paths passed for EchoEar and other boards.'

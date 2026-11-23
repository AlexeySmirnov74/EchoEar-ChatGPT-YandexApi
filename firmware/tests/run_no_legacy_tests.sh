#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
g++ -std=c++17 -Wall -Wextra -Werror "$ROOT/tests/test_no_legacy_policy.cpp" -o "$OUT/policy"
"$OUT/policy"
g++ -std=c++17 -Wall -Wextra -Werror -I"$ROOT/tests/nvs_fake" "$ROOT/tests/test_legacy_mqtt_nvs.cpp" -o "$OUT/nvs"
"$OUT/nvs"
cp "$ROOT/main/deployment_endpoint.h" "$ROOT/main/endpoint_policy.h" "$OUT/"
cp "$ROOT/tests/deployment_fake_settings.h" "$OUT/settings.h"
g++ -std=c++17 -Wall -Wextra -Werror -I"$OUT" "$ROOT/tests/test_deployment_endpoint.cpp" -o "$OUT/deployment"
"$OUT/deployment"

#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
g++ -std=c++17 -Wall -Wextra -Werror "$ROOT/tests/test_rest_policy.cpp" -o "$OUT/policy"
"$OUT/policy"
cat "$ROOT/tests/rest_backlight_prelude.cpp" > "$OUT/backlight.cpp"
sed -n '/^void Backlight::SetBrightness(/,/^PwmBacklight::PwmBacklight(/p' "$ROOT/main/boards/common/backlight.cc" | sed '$d' >> "$OUT/backlight.cpp"
cat "$ROOT/tests/rest_backlight_main.cpp" >> "$OUT/backlight.cpp"
g++ -std=c++17 -Wall -Wextra -Werror -pthread "$OUT/backlight.cpp" -o "$OUT/backlight"
"$OUT/backlight"
cat "$ROOT/tests/rest_queue_prelude.cpp" > "$OUT/queue.cpp"
sed -n '/^bool AudioService::PushPacketToDecodeQueue(/,/^void AudioService::SetRemoteAudioBlocked(/p' "$ROOT/main/audio/audio_service.cc" | sed '$d' >> "$OUT/queue.cpp"
cat "$ROOT/tests/rest_queue_main.cpp" >> "$OUT/queue.cpp"
g++ -std=c++17 -Wall -Wextra -Werror -pthread "$OUT/queue.cpp" -o "$OUT/queue"
"$OUT/queue"
cat "$ROOT/tests/rest_mcp_prelude.cpp" > "$OUT/mcp.cpp"
sed -n '/^WebsocketProtocol::McpSendResult WebsocketProtocol::SendRestMcpReply(/,/^bool WebsocketProtocol::SendTextForImageEpoch(/p' "$ROOT/main/protocols/websocket_protocol.cc" | sed '$d' >> "$OUT/mcp.cpp"
sed -n '/^bool Application::CanExecuteRestMcp(/,/^void Application::SendImageBackgroundAck(/p' "$ROOT/main/application.cc" | sed '$d' >> "$OUT/mcp.cpp"
cat "$ROOT/tests/rest_mcp_main.cpp" >> "$OUT/mcp.cpp"
g++ -std=c++17 -Wall -Wextra -Werror -pthread "$OUT/mcp.cpp" -o "$OUT/mcp"
"$OUT/mcp"

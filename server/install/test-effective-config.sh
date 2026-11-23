#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir -p "$test_dir/data" "$test_dir/secrets"
printf '%s' 'FAKE_OPENAI_KEY_FOR_TEST' > "$test_dir/secrets/openai_key"
cat > "$test_dir/fake-server" <<'EOF'
#!/bin/sh
set -eu
cfg=/app/manifest/config/config.yaml
grep -q 'ha_enabled: false' "$cfg"
grep -q 'standalone_ha_enabled: false' "$cfg"
grep -q 'ota_https_enabled: false' "$cfg"
grep -q 'conversation_history_enabled: false' "$cfg"
grep -q 'experimental_manual_enabled' /data/stackchan-settings.json
if grep -Eiq 'api\.tenclass\.net|xiaozhi\.me' "$cfg"; then exit 1; fi
grep -q 'FAKE_OPENAI_KEY_FOR_TEST' "$cfg"
printf 'Effective standalone configuration verified.\n'
EOF
chmod 755 "$test_dir/fake-server"
docker run --rm --network none --entrypoint /bin/bash \
  -e STACKCHAN_LOCAL_HOST=127.0.0.1 -e STACKCHAN_DATA_DIR=/data \
  -e STACKCHAN_STANDALONE_HA_ENABLED=false -e STACKCHAN_CONVERSATION_HISTORY_ENABLED=false \
  -v "$script_dir/standalone.sh:/source-standalone.sh:ro" \
  -v "$script_dir/voice-entrypoint.sh:/source-entrypoint.sh:ro" \
  -v "$test_dir/fake-server:/app/stackchan-server:ro" \
  -v "$test_dir/secrets:/run/secrets:ro" -v "$test_dir/data:/data" \
  "${ECHOWEAR_TEST_IMAGE:-python:3.12-slim-bookworm}" -c \
  'cp /source-standalone.sh /standalone.sh; cp /source-entrypoint.sh /voice-entrypoint.sh; chmod 755 /standalone.sh /voice-entrypoint.sh; exec /voice-entrypoint.sh'

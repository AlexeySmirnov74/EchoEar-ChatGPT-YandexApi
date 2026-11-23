#!/usr/bin/env bash
set -euo pipefail
test_dir=$(mktemp -d)
name="echowear-test-voice-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf -- "$test_dir"
}
trap cleanup EXIT
mkdir -p "$test_dir/data"
printf '%s' 'FAKE_OPENAI_KEY_FOR_TEST' > "$test_dir/openai_key"
docker run -d --name "$name" --network none \
  -e STACKCHAN_LOCAL_HOST=127.0.0.1 -e STACKCHAN_DATA_DIR=/data \
  -e STACKCHAN_CONVERSATION_HISTORY_ENABLED=false \
  -e STACKCHAN_STANDALONE_HA_ENABLED=false \
  -v "$test_dir/data:/data" -v "$test_dir/openai_key:/run/secrets/openai_key:ro" \
  "${ECHOWEAR_VOICE_IMAGE:-echowear-merge-test_voice}" >/dev/null
ready=0
for _ in $(seq 1 30); do
  if docker exec "$name" sh -c "netstat -ltn | grep -q ':12800 '"; then ready=1; break; fi
  sleep 0.5
done
if [[ $ready -ne 1 ]]; then
  echo 'Voice runtime did not start listening.' >&2
  docker logs "$name" >&2
  exit 1
fi
docker exec "$name" sh -c "grep -q 'ha_enabled: false' /app/manifest/config/config.yaml && grep -q 'ota_https_enabled: false' /app/manifest/config/config.yaml && ! grep -Eiq 'api[.]tenclass[.]net|xiaozhi[.]me' /app/manifest/config/config.yaml"
echo 'Voice runtime started without network using safe standalone settings.'

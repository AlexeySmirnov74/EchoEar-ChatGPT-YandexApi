#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
test_dir=$(mktemp -d)
voice="echowear-test-e2e-voice-$$"
gateway="echowear-test-e2e-gateway-$$"
mock="echowear-test-e2e-mock-$$"
cleanup() {
  docker rm -f "$gateway" "$mock" "$voice" >/dev/null 2>&1 || true
  rm -rf -- "$test_dir"
}
trap cleanup EXIT
mkdir -p "$test_dir/data" "$test_dir/credentials" "$test_dir/certs"
printf '%s' 'FAKE_OPENAI_KEY_FOR_TEST' > "$test_dir/openai_key"
printf '%s\n' 'ECHOWEAR_DOMAIN=localhost' > "$test_dir/config.env"
openssl rand -hex 32 > "$test_dir/credentials/ota_secret"
openssl rand -hex 32 > "$test_dir/credentials/ws_token"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$test_dir/certs/key.pem" -out "$test_dir/certs/cert.pem" -subj '/CN=localhost' -days 1 >/dev/null 2>&1
docker run -d --name "$voice" --network none \
  -e STACKCHAN_LOCAL_HOST=127.0.0.1 -e STACKCHAN_DATA_DIR=/data \
  -e STACKCHAN_OPENAI_BASE_URL=ws://127.0.0.1:19090/v1/realtime \
  -e STACKCHAN_STANDALONE_HA_ENABLED=false -e STACKCHAN_CONVERSATION_HISTORY_ENABLED=false \
  -v "$test_dir/data:/data" -v "$test_dir/openai_key:/run/secrets/openai_key:ro" \
  "${ECHOWEAR_VOICE_IMAGE:-echowear-merge-test_voice}" >/dev/null
docker run -d --name "$mock" --network "container:$voice" \
  -v "$script_dir/mock-realtime.py:/test/mock-realtime.py:ro" --entrypoint python \
  "${ECHOWEAR_TEST_IMAGE:-python:3.12-slim-bookworm}" /test/mock-realtime.py >/dev/null
docker run -d --name "$gateway" --network "container:$voice" \
  -e CREDENTIALS_DIRECTORY=/run/credentials -e TLS_CERT_PATH=/run/certs/cert.pem \
  -e TLS_KEY_PATH=/run/certs/key.pem -e GATEWAY_HOSTNAME=localhost \
  -e GATEWAY_PORT=18444 -e GATEWAY_BIND_ADDRESS=127.0.0.1 \
  -e UPSTREAM_PORT=12800 -v "$test_dir/credentials:/run/credentials:ro" -v "$test_dir/certs:/run/certs:ro" \
  "${ECHOWEAR_GATEWAY_IMAGE:-echowear-merge-test_gateway}" >/dev/null
ready=0
for _ in $(seq 1 30); do
  if docker exec "$voice" sh -c "netstat -ltn | grep -q ':18444 ' && netstat -ltn | grep -q ':12800 ' && netstat -ltn | grep -q ':19090 '"; then ready=1; break; fi
  sleep 0.5
done
if [[ $ready -ne 1 ]]; then
  echo 'Offline test services did not start.' >&2
  docker logs "$gateway" >&2
  docker logs "$voice" >&2
  exit 1
fi
docker run --rm --network "container:$voice" -e SSL_CERT_FILE=/run/certs/cert.pem \
  -v "$test_dir/credentials:/etc/echowear/gateway:ro" -v "$test_dir/config.env:/etc/echowear/config.env:ro" \
  -v "$test_dir/certs:/run/certs:ro" -v "$script_dir/verify-bootstrap.py:/test/verify-bootstrap.py:ro" \
  --entrypoint python "${ECHOWEAR_TEST_IMAGE:-python:3.12-slim-bookworm}" /test/verify-bootstrap.py
echo 'Verified installer TLS/OTA health check against the offline gateway.'
if ! docker run --rm --network "container:$voice" -v "$test_dir/credentials:/run/credentials:ro" \
  -v "$script_dir/check-gateway-e2e.py:/test/check.py:ro" --entrypoint python \
  "${ECHOWEAR_TEST_IMAGE:-python:3.12-slim-bookworm}" /test/check.py; then
  docker logs --tail 20 "$mock" >&2
  docker logs --tail 20 "$voice" >&2
  exit 1
fi

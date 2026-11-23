#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
test_dir=$(mktemp -d)
suffix=$$
admin="echowear-admin-e2e-$suffix"
gateway="echowear-gateway-e2e-$suffix"
search="echowear-search-e2e-$suffix"
cleanup() {
  docker rm -f "$admin" "$gateway" "$search" >/dev/null 2>&1 || true
  rm -rf -- "$test_dir"
}
trap cleanup EXIT
chmod 0755 "$test_dir"
install -d -o 10001 -g 10001 -m 0700 "$test_dir/state"
install -d -o 10001 -g 0 -m 2770 "$test_dir/admin-socket" "$test_dir/search-socket"
mkdir -p "$test_dir/credentials" "$test_dir/certs"
openssl rand -hex 32 > "$test_dir/credentials/ota_secret"
openssl rand -hex 32 > "$test_dir/credentials/ws_token"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$test_dir/certs/key.pem" -out "$test_dir/certs/cert.pem" -subj '/CN=unit.example' -days 1 >/dev/null 2>&1
python_image=${ECHOWEAR_PYTHON_IMAGE:-echowear-search}
node_image=${ECHOWEAR_NODE_IMAGE:-echowear-gateway}
docker run --pull never --rm --network none --user 10001:10001 -v "$root/admin:/app:ro" -v "$test_dir/state:/state" -w /app --entrypoint python "$python_image" -c \
  'import passwords,secret_store; passwords.create("/state/password.json","owner","test password long enough"); secret_store.write_bundle("/state/yandex.json","folder_first","FAKE_YANDEX_CREDENTIAL_FIRST_A")'
cp "$test_dir/state/password.json" "$test_dir/password.json"
chown 10001:10001 "$test_dir/password.json"
chmod 0400 "$test_dir/password.json"
rm "$test_dir/state/password.json"
docker run --pull never -d --name "$admin" --network none --cap-drop ALL --security-opt no-new-privileges --user 10001:10001 \
  -e ECHOWEAR_ADMIN_ORIGIN=https://unit.example:18444 -e PYTHONDONTWRITEBYTECODE=1 \
  -v "$root/admin:/app:ro" -v "$test_dir/state:/var/lib/echowear-admin" \
  -v "$test_dir/password.json:/run/secrets/admin_password_hash:ro" \
  -v "$test_dir/admin-socket:/run/echowear-admin" \
  --entrypoint python "$python_image" -I /app/app.py >/dev/null
docker run --pull never -d --name "$search" --network none --cap-drop ALL --security-opt no-new-privileges --user 10001:10001 \
  -e ECHOWEAR_YANDEX_BUNDLE=/state/yandex.json -e PYTHONDONTWRITEBYTECODE=1 \
  -v "$root/image-search:/app:ro" -v "$test_dir/state:/state:ro" \
  -v "$test_dir/search-socket:/run/echowear-images" --entrypoint python "$python_image" -I /app/mock_rotation_provider.py >/dev/null
docker run --pull never -d --name "$gateway" --network none --cap-drop ALL --security-opt no-new-privileges \
  -e CREDENTIALS_DIRECTORY=/run/credentials -e TLS_CERT_PATH=/run/certs/cert.pem -e TLS_KEY_PATH=/run/certs/key.pem \
  -e GATEWAY_HOSTNAME=unit.example -e GATEWAY_PORT=18444 -e GATEWAY_BIND_ADDRESS=0.0.0.0 -e UPSTREAM_PORT=12800 \
  -e ADMIN_SOCKET_PATH=/run/echowear-admin/admin.sock \
  -v "$root/gateway:/app:ro" -v "$test_dir/credentials:/run/credentials:ro" \
  -v "$test_dir/certs:/run/certs:ro" -v "$test_dir/admin-socket:/run/echowear-admin:ro" \
  --entrypoint node "$node_image" /app/gateway-alarm-clock.js >/dev/null
ready=0
for _ in $(seq 1 40); do
  if [[ -S "$test_dir/admin-socket/admin.sock" && -S "$test_dir/search-socket/search.sock" ]] && \
     docker exec "$gateway" sh -c "netstat -ltn | grep -q ':18444 '"; then ready=1; break; fi
  sleep 0.25
done
if [[ $ready -ne 1 ]]; then
  echo 'Test containers did not start.' >&2
  docker logs --tail 10 "$admin" >&2
  docker logs --tail 10 "$gateway" >&2
  exit 1
fi
before=$(docker inspect --format '{{.Id}}:{{.State.StartedAt}}' "$search")
docker exec "$search" python -I /app/check_rotation.py 17
docker run --pull never --rm --network "container:$gateway" -v "$root/admin/check_https_panel.py:/test/check.py:ro" \
  -v "$test_dir/certs:/run/certs:ro" --entrypoint python "$python_image" /test/check.py
docker exec "$search" python -I /app/check_rotation.py 34
after=$(docker inspect --format '{{.Id}}:{{.State.StartedAt}}' "$search")
[[ $before == "$after" ]]
if docker run --pull never --rm --network none --user 10001:10001 \
  -e ECHOWEAR_ADMIN_ORIGIN=https://unit.example:18444 \
  -v "$root/admin:/app:ro" -v "$test_dir/state:/var/lib/echowear-admin" \
  -v "$test_dir/password.json:/run/secrets/admin_password_hash:ro" \
  -v "$test_dir/admin-socket:/run/echowear-admin" \
  --entrypoint python "$python_image" -I /app/app.py >/dev/null 2>&1; then
  echo 'A second admin process unexpectedly acquired the lock.' >&2
  exit 1
fi
docker rm -f "$admin" >/dev/null
docker run --pull never -d --name "$admin" --network none --user 10001:10001 \
  -e ECHOWEAR_ADMIN_ORIGIN=https://unit.example:18444 \
  -v "$root/admin:/app:ro" -v "$test_dir/state:/var/lib/echowear-admin" \
  -v "$test_dir/password.json:/run/secrets/admin_password_hash:ro" \
  -v "$test_dir/admin-socket:/run/echowear-admin" \
  --entrypoint python "$python_image" -I /app/app.py >/dev/null
for _ in $(seq 1 20); do
  docker exec "$admin" python -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/echowear-admin/admin.sock")' >/dev/null 2>&1 && break
  sleep 0.25
done
docker exec "$admin" python -c 'import socket;s=socket.socket(socket.AF_UNIX);s.connect("/run/echowear-admin/admin.sock")'
echo 'Panel restart handled a stale socket; simultaneous startup was refused.'

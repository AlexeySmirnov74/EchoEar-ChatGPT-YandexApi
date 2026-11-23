#!/usr/bin/env bash
# Local synthetic test; no host ports, no real credentials or network access.
set -euo pipefail
export TEST_LAB_DIR
TEST_LAB_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
export TEST_DIR
TEST_DIR=$(mktemp -d /tmp/echowear-admin-canary.XXXXXXXX)
export TEST_NODE_IMAGE=${ECHOWEAR_NODE_IMAGE:-echowear-gateway}
export TEST_PYTHON_IMAGE=${ECHOWEAR_PYTHON_IMAGE:-echowear-search}
project="echowear-admin-canary-$$"
compose=(docker compose --project-name "$project" --file "$TEST_LAB_DIR/compose.canary.yaml")
cleanup() {
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
  case "$TEST_DIR" in /tmp/echowear-admin-canary.*) rm -rf -- "$TEST_DIR";; esac
}
trap cleanup EXIT
chmod 0755 "$TEST_DIR"
install -d -o 10001 -g 10001 -m 0700 "$TEST_DIR/state"
install -d -o 10001 -g 0 -m 2770 "$TEST_DIR/admin-socket" "$TEST_DIR/search-socket"
mkdir -p "$TEST_DIR/credentials" "$TEST_DIR/certs"
openssl rand -hex 32 > "$TEST_DIR/credentials/ota_secret"
openssl rand -hex 32 > "$TEST_DIR/credentials/ws_token"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$TEST_DIR/certs/key.pem" -out "$TEST_DIR/certs/cert.pem" -subj '/CN=unit.example' -days 1 >/dev/null 2>&1
docker run --pull never --rm --network none --user 10001:10001 -v "$TEST_LAB_DIR/admin:/app:ro" -v "$TEST_DIR/state:/state" -w /app --entrypoint python "$TEST_PYTHON_IMAGE" -c \
  'import passwords,secret_store; passwords.create("/state/password.json","owner","test password long enough"); secret_store.write_bundle("/state/yandex.json","folder_first","FAKE_YANDEX_CREDENTIAL_FIRST_A")'
cp "$TEST_DIR/state/password.json" "$TEST_DIR/password.json"
chown 10001:10001 "$TEST_DIR/password.json"
chmod 0400 "$TEST_DIR/password.json"
rm "$TEST_DIR/state/password.json"
"${compose[@]}" config --quiet
docker image inspect "$TEST_NODE_IMAGE" "$TEST_PYTHON_IMAGE" >/dev/null
"${compose[@]}" up --no-build -d >/dev/null
gateway=$("${compose[@]}" ps -q gateway)
search=$("${compose[@]}" ps -q search)
for _ in $(seq 1 40); do
  if [[ -S "$TEST_DIR/admin-socket/admin.sock" && -S "$TEST_DIR/search-socket/search.sock" ]] && \
      docker exec "$gateway" sh -c "netstat -ltn | grep -q ':18444 '"; then break; fi
  sleep 0.25
done
docker exec "$search" python -I /app/check_rotation.py 17
before=$(docker inspect --format '{{.Id}}:{{.State.StartedAt}}' "$search")
docker run --pull never --rm --network "container:$gateway" -v "$TEST_LAB_DIR/admin/check_https_panel.py:/test/check.py:ro" \
  -v "$TEST_DIR/certs:/run/certs:ro" --entrypoint python "$TEST_PYTHON_IMAGE" /test/check.py
docker exec "$search" python -I /app/check_rotation.py 34
after=$(docker inspect --format '{{.Id}}:{{.State.StartedAt}}' "$search")
[[ $before == "$after" ]]
echo 'Independent Compose canary passed, using no host ports or external network.'

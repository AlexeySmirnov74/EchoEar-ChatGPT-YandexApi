#!/usr/bin/env bash
set -euo pipefail
test_dir=$(mktemp -d)
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
search="echowear-test-search-$$"
decoder="echowear-test-decoder-$$"
cleanup() {
  docker rm -f "$search" "$decoder" >/dev/null 2>&1 || true
  rm -rf -- "$test_dir"
}
trap cleanup EXIT
install -d -o 10001 -g 0 -m 2770 "$test_dir/search" "$test_dir/decoder"
printf '%s' 'FAKE_YANDEX_KEY_FOR_TEST' > "$test_dir/yandex_key"
chown 10001:10001 "$test_dir/yandex_key"
chmod 0400 "$test_dir/yandex_key"
docker run -d --name "$decoder" --network none --user 10001:10001 \
  -v "$test_dir/decoder:/run/echowear" "${ECHOWEAR_DECODER_IMAGE:-echowear-merge-test_decoder}" >/dev/null
docker run -d --name "$search" --network none --user 10001:10001 \
  -e YANDEX_FOLDER_ID=folderexample \
  -v "$test_dir/decoder:/run/echowear" -v "$test_dir/search:/run/echowear-images" \
  -v "$test_dir/yandex_key:/run/secrets/yandex_key:ro" \
  -v "$script_dir/mock-image-provider.py:/test/mock-image-provider.py:ro" \
  -v "$script_dir/check-image-e2e.py:/test/check-image-e2e.py:ro" \
  --entrypoint python "${ECHOWEAR_SEARCH_IMAGE:-echowear-merge-test_search}" -I /test/mock-image-provider.py >/dev/null
ready=0
for _ in $(seq 1 30); do
  if [[ -S "$test_dir/decoder/decoder.sock" && -S "$test_dir/search/search.sock" ]]; then ready=1; break; fi
  sleep 0.5
done
if [[ $ready -ne 1 ]]; then
  echo 'Image service sockets did not appear.' >&2
  docker logs "$decoder" >&2
  docker logs "$search" >&2
  exit 1
fi
docker exec "$search" python -c 'import socket,struct; s=socket.socket(socket.AF_UNIX); s.connect("/run/echowear/decoder.sock"); s.sendall(struct.pack("!I",0)); assert s.recv(4)==struct.pack("!I",0)'
docker exec "$search" python -I /test/check-image-e2e.py

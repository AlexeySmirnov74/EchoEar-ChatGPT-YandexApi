#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
cfg=/etc/echowear/config.env
for attempt in $(seq 1 "${ECHOWEAR_VERIFY_ATTEMPTS:-30}"); do
  ready=true
  for service in voice gateway search decoder admin; do
    id=$(docker compose --env-file "$cfg" -f "$script_dir/compose.yaml" ps -q "$service") || ready=false
    if [[ -z ${id:-} || $(docker inspect --format '{{.State.Running}}' "$id" 2>/dev/null || true) != true ]]; then
      ready=false
    fi
  done
  if [[ $ready == true ]] && ss -H -ltn 'sport = :18444' | grep -q . && python3 "$script_dir/verify-bootstrap.py" >/dev/null 2>&1 && python3 "$script_dir/verify-admin.py" >/dev/null 2>&1; then
    echo 'Все пять контейнеров запущены; TLS, OTA и доступность панели проверены.'
    exit 0
  fi
  sleep 1
done
echo 'Контейнеры или проверка TLS/OTA не готовы. Проверьте сертификат и docker compose ps.' >&2
exit 1

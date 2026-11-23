#!/usr/bin/env bash
# Resume a partial install without deleting data or changing existing secrets.
set -euo pipefail
set +x
umask 077
if [[ $EUID -ne 0 ]]; then echo 'Запустите от root.' >&2; exit 1; fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
owner=/etc/echowear/install-owner
if [[ ! -f $owner || -L $owner || $(stat -c '%u:%g:%a' "$owner") != 0:0:600 || $(cat "$owner") != ECHOWEAR_INSTALL_V1 ]]; then
  echo 'Нет проверенного маркера установки EchoWear; восстановление отменено.' >&2
  exit 1
fi
cfg=/etc/echowear/config.env
if [[ ! -f $cfg || -L $cfg || $(stat -c '%u:%g:%a' "$cfg") != 0:0:600 ]]; then echo 'Нет защищённого файла параметров; требуется ручная диагностика.' >&2; exit 1; fi
domain=$(sed -n 's/^ECHOWEAR_DOMAIN=//p' "$cfg")
folder=$(sed -n 's/^YANDEX_FOLDER_ID=//p' "$cfg")
if [[ $(wc -l < "$cfg") -ne 2 || ! $domain =~ ^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$ || $domain != *.* || ! $folder =~ ^[A-Za-z0-9_-]{8,100}$ ]]; then
  echo 'Не удалось проверить параметры; восстановление отменено.' >&2
  exit 1
fi
for item in /etc/echowear/openai_key /etc/echowear/yandex_key /etc/echowear/gateway/ota_secret /etc/echowear/gateway/ws_token; do
  if [[ ! -s $item || -L $item || $(stat -c %a "$item") != 400 ]]; then
    echo 'Один из файлов ключей отсутствует или имеет неверные права; нужна ручная диагностика.' >&2
    exit 1
  fi
done
if [[ $(stat -c '%u:%g' /etc/echowear/openai_key) != 0:0 || $(stat -c '%u:%g' /etc/echowear/yandex_key) != 10001:10001 || $(stat -c '%u:%g' /etc/echowear/gateway/ota_secret) != 0:0 || $(stat -c '%u:%g' /etc/echowear/gateway/ws_token) != 0:0 ]]; then
  echo 'Владельцы ключей отличаются от ожидаемых; нужна ручная диагностика.' >&2
  exit 1
fi
for dir in /var/lib/echowear/data /var/lib/echowear/decoder-socket /var/lib/echowear/search-socket; do
  if [[ ! -d $dir || -L $dir ]]; then echo 'Один из каталогов данных отсутствует; нужна ручная диагностика.' >&2; exit 1; fi
done
for dir in /var/lib/echowear/decoder-socket /var/lib/echowear/search-socket; do
  if [[ $(stat -c '%u:%g:%a' "$dir") != 10001:0:2770 ]]; then
    echo 'Права каталога сокетов отличаются от ожидаемых; нужна ручная диагностика.' >&2
    exit 1
  fi
done
if [[ ! -s /etc/letsencrypt/live/$domain/fullchain.pem || ! -s /etc/letsencrypt/live/$domain/privkey.pem ]]; then
  echo 'Сертификат не найден; сначала восстановите его через certbot.' >&2
  exit 1
fi
for service in gateway voice search decoder; do
  names=("echowear-$service-1" "echowear_${service}_1")
  [[ $service == gateway ]] && names=(echowear-gateway)
  for name in "${names[@]}"; do
    if docker inspect "$name" >/dev/null 2>&1; then
      project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$name")
      kind=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$name")
      if [[ $project != echowear || $kind != "$service" ]]; then
        echo "Контейнер $name принадлежит другой системе; восстановление отменено." >&2
        exit 1
      fi
    fi
  done
done
hook=/etc/letsencrypt/renewal-hooks/deploy/echowear-reload
if [[ -e $hook || -L $hook ]]; then
  if [[ -L $hook || $(sha256sum "$hook" | cut -d' ' -f1) != $(printf '#!/bin/sh\ndocker restart echowear-gateway >/dev/null\n' | sha256sum | cut -d' ' -f1) ]]; then
    echo 'Существующий certbot hook отличается; он не будет изменён.' >&2
    exit 1
  fi
else
  install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
  printf '#!/bin/sh\ndocker restart echowear-gateway >/dev/null\n' > "$hook"
  chmod 0755 "$hook"
fi
systemctl enable --now certbot.timer
docker compose --env-file "$cfg" -f "$script_dir/compose.yaml" up -d --build
if ! bash "$script_dir/verify-install.sh"; then
  echo 'Восстановление не подтверждено. Проверьте сертификат и docker compose ps.' >&2
  exit 1
fi
echo 'Восстановление завершено; состояние: docker compose ps.'

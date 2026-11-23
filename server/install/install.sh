#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

if [[ $EUID -ne 0 ]]; then
  echo 'Запустите установку от root: sudo bash install.sh' >&2
  exit 1
fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
if [[ -e /etc/echowear || -e /var/lib/echowear ]]; then
  echo 'Найдены существующие данные EchoWear. Установка не перезаписывает их.' >&2
  exit 1
fi
if [[ -e /etc/letsencrypt/renewal-hooks/deploy/echowear-reload ]]; then
  echo 'Обнаружен существующий certbot hook; установка не будет его менять.' >&2
  exit 1
fi
read -r -p 'Домен колонки (например, echo.example.com): ' domain
if [[ ! $domain =~ ^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$ || $domain != *.* || $domain == *..* || ${#domain} -gt 253 ]]; then
  echo 'Некорректный домен.' >&2
  exit 1
fi
read -r -p 'Email для Let’s Encrypt: ' email
if [[ ! $email =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; then
  echo 'Некорректный email.' >&2
  exit 1
fi
read -r -p 'Идентификатор каталога Yandex Cloud: ' folder
if [[ ! $folder =~ ^[A-Za-z0-9_-]{8,100}$ ]]; then
  echo 'Некорректный идентификатор каталога.' >&2
  exit 1
fi
read -r -s -p 'Ключ OpenAI API: ' openai_key
echo
read -r -s -p 'Ключ Yandex Search API: ' yandex_key
echo
if [[ ! $openai_key =~ ^sk-[A-Za-z0-9_-]{20,}$ || ! $yandex_key =~ ^[A-Za-z0-9_-]{20,}$ ]]; then
  echo 'Неверный формат ключа.' >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates certbot docker.io docker-compose-v2 iproute2 openssl python3
systemctl enable --now docker
docker compose version >/dev/null
for name in echowear-gateway echowear-voice-1 echowear-search-1 echowear-decoder-1 echowear_voice_1 echowear_search_1 echowear_decoder_1; do
  if docker inspect "$name" >/dev/null 2>&1; then
    echo "Уже существует контейнер $name; установка не будет его изменять." >&2
    exit 1
  fi
done
for port in 80 12800 18444; do
  if ss -H -ltn "sport = :$port" | grep -q .; then
    echo "Порт $port занят; освободите его и повторите установку." >&2
    exit 1
  fi
done
certbot certonly --standalone --non-interactive --agree-tos --email "$email" -d "$domain"

install -d -m 0700 /etc/echowear /etc/echowear/gateway /var/lib/echowear /var/lib/echowear/data
printf '%s\n' 'ECHOWEAR_INSTALL_V1' > /etc/echowear/install-owner
chmod 0600 /etc/echowear/install-owner
install -d -o 10001 -g 0 -m 2770 /var/lib/echowear/decoder-socket /var/lib/echowear/search-socket
printf '%s' "$openai_key" > /etc/echowear/openai_key
printf '%s' "$yandex_key" > /etc/echowear/yandex_key
unset openai_key yandex_key
chmod 0400 /etc/echowear/openai_key /etc/echowear/yandex_key
chown 10001:10001 /etc/echowear/yandex_key
openssl rand -hex 32 > /etc/echowear/gateway/ota_secret
openssl rand -hex 32 > /etc/echowear/gateway/ws_token
chmod 0400 /etc/echowear/gateway/ota_secret /etc/echowear/gateway/ws_token
printf 'ECHOWEAR_DOMAIN=%s\nYANDEX_FOLDER_ID=%s\n' "$domain" "$folder" > /etc/echowear/config.env
chmod 0600 /etc/echowear/config.env

install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
cat > /etc/letsencrypt/renewal-hooks/deploy/echowear-reload <<'EOF'
#!/bin/sh
docker restart echowear-gateway >/dev/null
EOF
chmod 0755 /etc/letsencrypt/renewal-hooks/deploy/echowear-reload
systemctl enable --now certbot.timer
docker compose --env-file /etc/echowear/config.env -f "$script_dir/compose.yaml" up -d --build
if ! bash "$script_dir/verify-install.sh"; then
  echo "Установка не подтверждена. После диагностики запустите: sudo bash $script_dir/recover.sh" >&2
  exit 1
fi
echo 'Контейнеры запущены. OTA-адрес можно показать командой:'
printf 'sudo bash %q\n' "$script_dir/show-ota.sh"

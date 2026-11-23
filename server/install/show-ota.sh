#!/usr/bin/env bash
set -euo pipefail
set +x
if [[ $EUID -ne 0 ]]; then echo 'Запустите от root.' >&2; exit 1; fi
domain=$(sed -n 's/^ECHOWEAR_DOMAIN=//p' /etc/echowear/config.env)
[[ $domain =~ ^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$ && $domain == *.* ]] || exit 1
printf 'https://%s:18444/' "$domain"
cat /etc/echowear/gateway/ota_secret
printf '\n'

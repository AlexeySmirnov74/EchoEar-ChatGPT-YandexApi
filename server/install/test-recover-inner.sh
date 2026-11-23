#!/usr/bin/env bash
set -euo pipefail
case_name=$1
mkdir -p /etc/echowear/gateway /var/lib/echowear/{data,decoder-socket,search-socket} /etc/letsencrypt/live/test.example /test/bin
chown 10001:0 /var/lib/echowear/{decoder-socket,search-socket}
chmod 2770 /var/lib/echowear/{decoder-socket,search-socket}
cat >/test/bin/docker <<'EOF'
#!/usr/bin/env bash
case "$1" in
  inspect)
    if [[ ${4:-} == fake-id ]]; then
      if [[ ${FAKE_RUNTIME_DOWN:-0} == 1 ]]; then echo false; else echo true; fi
      exit 0
    fi
    if [[ ${FAKE_COLLISION:-0} == 1 ]]; then
      if [[ ${2:-} == --format ]]; then echo foreign; fi
      exit 0
    fi
    exit 1;;
  compose)
    if [[ " $* " == *" ps -q "* ]]; then echo fake-id; else echo compose >> /test/actions; fi;;
  *) exit 1;;
esac
EOF
cat >/test/bin/systemctl <<'EOF'
#!/bin/sh
echo systemctl >> /test/actions
EOF
chmod 755 /test/bin/docker /test/bin/systemctl
cat >/test/bin/ss <<'EOF'
#!/bin/sh
echo 'LISTEN 0 10 127.0.0.1:18444 0.0.0.0:*'
EOF
cat >/test/bin/python3 <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 755 /test/bin/ss /test/bin/python3
export PATH="/test/bin:$PATH"
if [[ $case_name != no-marker ]]; then printf '%s\n' ECHOWEAR_INSTALL_V1 >/etc/echowear/install-owner; chmod 600 /etc/echowear/install-owner; fi
if [[ $case_name != incomplete ]]; then
  printf 'ECHOWEAR_DOMAIN=test.example\nYANDEX_FOLDER_ID=folderexample\n' >/etc/echowear/config.env
  chmod 600 /etc/echowear/config.env
  for f in /etc/echowear/openai_key /etc/echowear/yandex_key /etc/echowear/gateway/ota_secret /etc/echowear/gateway/ws_token; do
    printf 'FAKE_SECRET' >"$f"
    chmod 400 "$f"
  done
  chown 10001:10001 /etc/echowear/yandex_key
  printf 'cert' >/etc/letsencrypt/live/test.example/fullchain.pem
  printf 'key' >/etc/letsencrypt/live/test.example/privkey.pem
fi
if [[ $case_name == collision ]]; then export FAKE_COLLISION=1; fi
if [[ $case_name == runtime-down ]]; then export FAKE_RUNTIME_DOWN=1 ECHOWEAR_VERIFY_ATTEMPTS=1; fi
if [[ $case_name == other-hook ]]; then
  mkdir -p /etc/letsencrypt/renewal-hooks/deploy
  printf 'foreign hook' >/etc/letsencrypt/renewal-hooks/deploy/echowear-reload
fi
if [[ $case_name == success ]]; then
  before=$(sha256sum /etc/echowear/{openai_key,yandex_key} /etc/echowear/gateway/{ota_secret,ws_token})
  bash /src/recover.sh
  after=$(sha256sum /etc/echowear/{openai_key,yandex_key} /etc/echowear/gateway/{ota_secret,ws_token})
  [[ $before == "$after" ]]
  grep -q compose /test/actions
  grep -q systemctl /test/actions
elif [[ $case_name == runtime-down ]]; then
  if bash /src/recover.sh >/dev/null 2>&1; then echo 'Unexpected success with stopped container' >&2; exit 1; fi
  grep -q compose /test/actions
else
  if bash /src/recover.sh >/dev/null 2>&1; then echo "Unexpected success: $case_name" >&2; exit 1; fi
  [[ ! -f /test/actions ]]
fi
echo "Recovery test passed: $case_name"

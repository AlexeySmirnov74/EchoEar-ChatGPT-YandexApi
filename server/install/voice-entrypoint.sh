#!/bin/sh
set -eu
umask 077
test -s /run/secrets/openai_key
if [ ! -e /data/stackchan-settings.json ]; then
  printf '%s\n' '{"experimental_manual_enabled":"true"}' > /data/stackchan-settings.json
fi
export STACKCHAN_OPENAI_API_KEY="$(cat /run/secrets/openai_key)"
exec /standalone.sh

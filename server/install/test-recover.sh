#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
for case_name in no-marker incomplete collision other-hook runtime-down success; do
  docker run --rm --network none --entrypoint /bin/bash \
    -v "$script_dir:/src:ro" "${ECHOWEAR_TEST_IMAGE:-python:3.12-slim-bookworm}" \
    /src/test-recover-inner.sh "$case_name"
done

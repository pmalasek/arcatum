#!/usr/bin/env bash
# Demo backup-like script. Non-secret params arrive as ARCATUM_<NAME>; the secret
# is sourced from ARCATUM_SECRETS_FILE (removed by the runner afterwards).
set -euo pipefail

echo "hello from instance name=${ARCATUM_NAME}"
echo "backing up target=${ARCATUM_TARGET}"

# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "${ARCATUM_SECRETS_FILE}"
echo "auth token prefix: ${ARCATUM_TOKEN:0:3}*** (len=${#ARCATUM_TOKEN})"

# Simulate streamed backup data going to the server (not stored locally).
for i in 1 2 3; do
  echo "chunk $i: $(printf 'x%.0s' {1..20})"
done
echo "done"

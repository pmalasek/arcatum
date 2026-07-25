#!/usr/bin/env bash
# Example script definition for Arcatum.
#
# Non-secret parameters arrive as ARCATUM_<PARAM> environment variables.
# Secrets (here: the DB password) are delivered in a short-lived config file whose
# path is in ARCATUM_SECRETS_FILE, and which the runner removes after the run.
#
# The dump is written to stdout so the runner can stream it to the server without
# storing it locally (capture = "stream").
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"
: "${ARCATUM_DATABASE:?missing database}"
: "${ARCATUM_USER:?missing user}"
PORT="${ARCATUM_PORT:-3306}"

# Secrets file is expected to define MYSQL_PWD (consumed by mysqldump via env).
# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"

exec mysqldump \
  --host="$ARCATUM_HOST" \
  --port="$PORT" \
  --user="$ARCATUM_USER" \
  --single-transaction --quick --routines --triggers \
  "$ARCATUM_DATABASE"

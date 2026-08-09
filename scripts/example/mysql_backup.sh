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

# The secrets file defines ARCATUM_<PARAM>, so the password arrives as
# ARCATUM_PASSWORD (see docs/script-development.md).
# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"
: "${ARCATUM_PASSWORD:?missing password}"

# mysqldump takes the password from MYSQL_PWD; --password= would expose it in the
# process list on the backed-up host.
export MYSQL_PWD="$ARCATUM_PASSWORD"

exec mysqldump \
  --host="$ARCATUM_HOST" \
  --port="$PORT" \
  --user="$ARCATUM_USER" \
  --single-transaction --quick --routines --triggers --events \
  "$ARCATUM_DATABASE"

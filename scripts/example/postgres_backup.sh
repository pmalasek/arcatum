#!/usr/bin/env bash
# Example script definition for Arcatum — the PostgreSQL counterpart of mysql_backup.sh.
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
PORT="${ARCATUM_PORT:-5432}"

# The secrets file defines ARCATUM_<PARAM>, so the password arrives as
# ARCATUM_PASSWORD (see docs/script-development.md).
# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"
: "${ARCATUM_PASSWORD:?missing password}"

# pg_dump reads the password from PGPASSWORD; every command-line alternative would
# expose it in the process list on the backed-up host.
export PGPASSWORD="$ARCATUM_PASSWORD"

# --format=plain is the mysqldump equivalent: text SQL, restored with
#   psql --dbname=<db> -f dump
# There is no counterpart to --single-transaction/--routines/--triggers: pg_dump is
# consistent by default (one repeatable-read snapshot) and functions and triggers are
# part of the dump already. --no-password turns a missing or wrong password into an
# immediate failure instead of a prompt nobody is there to answer.
exec pg_dump \
  --host="$ARCATUM_HOST" \
  --port="$PORT" \
  --username="$ARCATUM_USER" \
  --no-password \
  --format=plain \
  --dbname="$ARCATUM_DATABASE"

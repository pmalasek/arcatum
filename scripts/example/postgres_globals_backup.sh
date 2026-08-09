#!/usr/bin/env bash
# Example script definition for Arcatum — the cluster-wide half of a PostgreSQL backup.
#
# postgres_backup.sh dumps one database. Roles, their passwords and tablespaces live
# outside any database and are in no such dump, yet a per-database dump refers to them
# in every ALTER ... OWNER TO. Restoring onto an empty cluster therefore needs both:
# one instance of this script per server, plus one postgres-backup instance per database.
#
# Non-secret parameters arrive as ARCATUM_<PARAM> environment variables; the password
# comes from the short-lived file in ARCATUM_SECRETS_FILE (docs/script-development.md).
# The dump is written to stdout so nothing stays on the backed-up host (capture = "stream").
set -euo pipefail

: "${ARCATUM_HOST:?missing host}"
: "${ARCATUM_USER:?missing user}"
PORT="${ARCATUM_PORT:-5432}"
MAINTENANCE_DB="${ARCATUM_MAINTENANCE_DATABASE:-postgres}"

# The secrets file defines ARCATUM_<PARAM>, so the password arrives as ARCATUM_PASSWORD.
# shellcheck disable=SC1090
[ -n "${ARCATUM_SECRETS_FILE:-}" ] && source "$ARCATUM_SECRETS_FILE"
: "${ARCATUM_PASSWORD:?missing password}"

# pg_dumpall reads the password from PGPASSWORD; every command-line alternative would
# expose it in the process list on the backed-up host.
export PGPASSWORD="$ARCATUM_PASSWORD"

# --globals-only: roles, tablespaces and cluster-wide grants, no database contents.
# pg_dumpall still has to connect somewhere to read them — --database says where, the
# "postgres" maintenance database on a stock cluster.
#
# Role passwords come from pg_authid, which in practice only a superuser may read: run
# this as anything less and pg_dumpall fails with "permission denied for table pg_authid"
# rather than quietly dumping roles without their passwords. That is the right way round
# — a globals backup missing every password would still look like a successful run. If a
# superuser is genuinely not available, add --no-role-passwords and know what you gave up.
exec pg_dumpall \
  --host="$ARCATUM_HOST" \
  --port="$PORT" \
  --username="$ARCATUM_USER" \
  --no-password \
  --database="$MAINTENANCE_DB" \
  --globals-only

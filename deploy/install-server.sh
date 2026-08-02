#!/usr/bin/env bash
# Install or update the Arcatum server on this machine: directories, binaries, script
# definitions, PKI, configuration and the systemd unit. Everything it finds already in
# place is left alone, so the first install and every later update are the same command.
#
# Usage:
#   sudo deploy/install-server.sh -H 172.24.0.60[,arcatum.example.local] [-a petr]
#
#   -H  hosts runners connect to (comma-separated DNS names and/or IPs). Needed the
#       first time, for the server certificate and for api_url in server.toml.
#   -a  admin certificate name (default: admin)
#   -b  backup_dir — where backup data lands (default: /central_backup/arcatum)
#   -p  install prefix (default: /opt/arcatum)
#   -n  dry run: print what would happen, change nothing
#
# It installs from the checkout it lives in: bin/ has to hold the binaries (just release,
# just dist-runner bin) and scripts/ the script definitions. Nothing is built here — a
# production server does not need Go.
#
# Everything lands under the prefix, binaries included; /usr/local/bin only gets symlinks
# to arcatum-server and arcatum-ca, which are the two that ever get typed by hand.
#
# Not touched once they exist: server.toml, the PKI, the systemd unit. Config and unit
# are yours to edit afterwards; re-running never reverts those edits.
set -euo pipefail

PREFIX="/opt/arcatum"
BACKUP_DIR="/central_backup/arcatum"
# System locations. Overridable from the environment so the whole installer can be
# exercised against a scratch directory instead of the machine it runs on.
CONFIG_DIR="${ARCATUM_CONFIG_DIR:-/etc/arcatum}"
UNIT="${ARCATUM_UNIT:-/etc/systemd/system/arcatum-server.service}"
# Binaries live under the install prefix, so the whole installation is one directory and
# no stale copy of a previous version can survive elsewhere. LINK_DIR gets symlinks to
# the two that are run by hand — arcatum-ca does PKI work and never runs as a service,
# and gen-certs.sh looks for it with command -v.
LINK_DIR="${ARCATUM_LINK_DIR:-/usr/local/bin}"
HOSTS=""
ADMIN_NAME="admin"
DRY=0

while getopts ":H:a:b:p:nh" opt; do
  case "$opt" in
    H) HOSTS="$OPTARG" ;;
    a) ADMIN_NAME="$OPTARG" ;;
    b) BACKUP_DIR="$OPTARG" ;;
    p) PREFIX="$OPTARG" ;;
    n) DRY=1 ;;
    h) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    :) echo "error: -$OPTARG needs a value" >&2; exit 2 ;;
    \?) echo "error: unknown option -$OPTARG" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG="$CONFIG_DIR/server.toml"
PKI="$PREFIX/pki"
DIST="$PREFIX/dist"
BIN_DIR="$PREFIX/bin"
NOTES=()

run() {
  if [ "$DRY" -eq 1 ]; then
    printf '     would: %s\n' "$*"
  else
    "$@"
  fi
}

fail() { echo "error: $*" >&2; exit 1; }

# --- what we install from -------------------------------------------------------------

[ -d "$REPO_ROOT/bin" ] || fail "no bin/ in $REPO_ROOT — build first (just release)"
[ -d "$REPO_ROOT/scripts" ] || fail "no scripts/ in $REPO_ROOT"
for b in arcatum-server arcatum-ca; do
  [ -f "$REPO_ROOT/bin/$b" ] || fail "bin/$b is missing — build it (just release)"
done

if [ "$DRY" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
  fail "run as root (sudo $0 …)"
fi

# The server shells out to restic for restores and repository sizes; without it those
# parts of the API fail at the moment somebody needs them most.
command -v restic >/dev/null 2>&1 || NOTES+=("restic is not installed — restores and repository sizes will fail (apt install restic)")

# A first install needs -H: the server certificate has to list every address runners
# use, and api_url is written into every generated runner.toml.
if [ ! -f "$PKI/ca.pem" ] || [ ! -f "$CONFIG" ]; then
  [ -n "$HOSTS" ] || fail "-H is required on a first install (e.g. -H 172.24.0.60)"
fi
PRIMARY_HOST="${HOSTS%%,*}"

echo "==> installing from $REPO_ROOT"
[ "$DRY" -eq 1 ] && echo "    (dry run)"

# --- directories ----------------------------------------------------------------------
# 0700 on pki/ before anything writes a private key into it: created by arcatum-ca it
# would be world-readable.

echo "==> directories"
run install -d -m 0755 "$CONFIG_DIR" "$PREFIX" "$BIN_DIR" "$DIST" "$PREFIX/scripts" "$BACKUP_DIR" "$LINK_DIR"
run install -d -m 0700 "$PKI"

# --- binaries -------------------------------------------------------------------------
# Installed side by side and renamed into place: writing over a running binary fails
# with "Text file busy", while a rename over it is fine.

echo "==> binaries into $BIN_DIR"
for b in arcatum-server arcatum-ca; do
  run install -m 0755 "$REPO_ROOT/bin/$b" "$BIN_DIR/$b.new"
  run mv "$BIN_DIR/$b.new" "$BIN_DIR/$b"
  # Symlink, not a copy: an update replaces the file the link points at, so the two can
  # never drift into different versions.
  run ln -sfn "$BIN_DIR/$b" "$LINK_DIR/$b"
done

# Runner builds are published, not installed: the server only hands them out. Without a
# VERSION file next to them nothing is offered for update, which is deliberate — copying
# binaries and releasing them are separate acts.
runners=("$REPO_ROOT"/bin/arcatum-runner-linux-*)
if [ -e "${runners[0]}" ]; then
  echo "==> runner builds into $DIST"
  run install -m 0755 "${runners[@]}" "$DIST/"
  if [ -f "$REPO_ROOT/bin/VERSION" ]; then
    run install -m 0644 "$REPO_ROOT/bin/VERSION" "$DIST/VERSION"
  else
    NOTES+=("no bin/VERSION — write $DIST/VERSION yourself, or runners are offered no update")
  fi
else
  NOTES+=("no bin/arcatum-runner-linux-* — build them (just dist-runner bin) or hosts cannot install a runner")
fi

# --- script definitions ---------------------------------------------------------------
# --delete matters on an update: a manifest deleted in the repository has to disappear
# here too, or the server keeps offering a script that no longer exists.

echo "==> script definitions into $PREFIX/scripts"
if command -v rsync >/dev/null 2>&1; then
  run rsync -a --delete "$REPO_ROOT/scripts/" "$PREFIX/scripts/"
else
  run cp -a "$REPO_ROOT/scripts/." "$PREFIX/scripts/"
  NOTES+=("rsync is missing, so scripts/ was copied without --delete — a manifest deleted in the repository still sits in $PREFIX/scripts")
fi

# --- PKI --------------------------------------------------------------------------------
# Before the first start of the server, and never on top of an existing CA: those keys
# are what every runner already trusts.

if [ -f "$PKI/ca.pem" ]; then
  echo "==> PKI exists in $PKI, keeping it"
else
  echo "==> PKI: CA, dispatch-signing key, secrets master key, certificates"
  run "$BIN_DIR/arcatum-ca" init -dir "$PKI"
  run "$BIN_DIR/arcatum-ca" server -dir "$PKI" -hosts "$HOSTS"
  run "$BIN_DIR/arcatum-ca" admin -dir "$PKI" -name "$ADMIN_NAME"
  NOTES+=("back up $PKI/secrets-master.key, ca.key and dispatch-signing.key off this machine — losing the master key makes every stored secret, and with it every repository, unreadable")
fi
if [ "$DRY" -eq 0 ] && [ -d "$PKI" ]; then
  chmod 600 "$PKI"/*.key 2>/dev/null || true
  chmod 644 "$PKI"/*.pem "$PKI"/*.pub 2>/dev/null || true
fi

# --- configuration ----------------------------------------------------------------------
# Found by the server itself: ./server.toml first, then /etc/arcatum/server.toml. The PKI
# lives under the install prefix, not in backup_dir — private keys have no business on the
# same volume as the repositories they unlock.

if [ -f "$CONFIG" ]; then
  echo "==> $CONFIG exists, keeping it"
else
  echo "==> writing $CONFIG"
  if [ "$DRY" -eq 1 ]; then
    printf '     would: write %s (listen 0.0.0.0:8443, api_url https://%s:8443)\n' "$CONFIG" "$PRIMARY_HOST"
  else
    cat > "$CONFIG" <<EOF
# Written by deploy/install-server.sh. Re-running the installer never touches it again.
# Reference with every option: config/server.example.toml in the repository.

[server]
listen    = "0.0.0.0:8443"
scripts   = "$PREFIX/scripts"
data_dir  = "$BACKUP_DIR/data"
timezone  = "Europe/Prague"
log_level = "info"

[web]
listen      = "0.0.0.0:8080"
session_ttl = "12h"

[storage]
backup_dir = "$BACKUP_DIR"

[tls]
ca_cert = "$PKI/ca.pem"
cert    = "$PKI/server.pem"
key     = "$PKI/server.key"

[signing]
key = "$PKI/dispatch-signing.key"

[secrets]
master_key = "$PKI/secrets-master.key"

[bootstrap]
listen   = "0.0.0.0:80"
dist_dir = "$DIST"
api_url  = "https://$PRIMARY_HOST:8443"
ca_key   = "$PKI/ca.key"
EOF
    chmod 640 "$CONFIG"
  fi
fi

# --- systemd unit -------------------------------------------------------------------------
# -config is named explicitly even though the search path would find it: the unit sets a
# working directory, and a server.toml dropped in there must not be able to override the
# configuration the service runs on.

if [ -f "$UNIT" ]; then
  echo "==> $UNIT exists, keeping it"
else
  echo "==> writing $UNIT"
  if [ "$DRY" -eq 1 ]; then
    printf '     would: write %s\n' "$UNIT"
  else
    cat > "$UNIT" <<EOF
[Unit]
Description=Arcatum backup server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_DIR/arcatum-server -config $CONFIG -instances /dev/null
WorkingDirectory=$PREFIX
Restart=always
RestartSec=5

# Needed only once the service runs as an unprivileged user: the bootstrap listener
# binds port 80. Harmless for root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=$BACKUP_DIR
ReadOnlyPaths=$PREFIX

[Install]
WantedBy=multi-user.target
EOF
  fi
  run systemctl daemon-reload
fi

# --- finish ---------------------------------------------------------------------------
# An update restarts the service; a first install does not. The first start belongs in
# the foreground, where the generated admin password is printed once and any mistake in
# the configuration is visible immediately.

STARTED=0
if [ "$DRY" -eq 0 ] && systemctl is-active --quiet arcatum-server; then
  echo "==> restarting arcatum-server"
  systemctl restart arcatum-server
  STARTED=1
fi

echo
echo "Done."
if [ ${#NOTES[@]} -gt 0 ]; then
  echo
  echo "Check:"
  printf '  - %s\n' "${NOTES[@]}"
fi
echo
if [ "$STARTED" -eq 1 ]; then
  cat <<EOF
Service restarted. The first log line says which configuration it read:

  journalctl -u arcatum-server -n 30
EOF
else
  cat <<EOF
Not started yet. Run it once in the foreground — the generated password of the "admin"
web account is printed there once, and a mistake in the configuration shows up right away
(Ctrl-C to stop):

  cd /root && arcatum-server -instances /dev/null

Look for "mTLS enabled" and "instance secrets are encrypted at rest", and for no WARNING
lines. Then hand it to systemd:

  systemctl enable --now arcatum-server
  journalctl -u arcatum-server -n 30
EOF
fi

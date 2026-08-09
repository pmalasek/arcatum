#!/usr/bin/env bash
# Generate a complete Arcatum PKI in one go: CA, dispatch-signing keypair, server
# certificate, an admin certificate, and one certificate per runner.
#
# Usage:
#   deploy/gen-certs.sh -H 172.24.0.60 [-d pki] [-a petr] [runner-id ...]
#
#   -H  hosts the server is reached at (comma-separated DNS names and/or IPs)
#   -d  output directory (default: pki)
#   -a  admin certificate name (default: admin)
#
# Example:
#   deploy/gen-certs.sh -H 172.24.0.60,arcatum.xtuning.local web-01 db-01
#
# Re-running is safe: an existing CA or signing key is never overwritten, and only
# the missing pieces are added. To rotate a runner certificate, delete its files first.
set -euo pipefail

PKI_DIR="pki"
HOSTS=""
ADMIN_NAME="admin"

while getopts ":H:d:a:h" opt; do
  case "$opt" in
    H) HOSTS="$OPTARG" ;;
    d) PKI_DIR="$OPTARG" ;;
    a) ADMIN_NAME="$OPTARG" ;;
    h) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    :) echo "error: -$OPTARG needs a value" >&2; exit 2 ;;
    \?) echo "error: unknown option -$OPTARG" >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))
RUNNERS=("$@")

if [ -z "$HOSTS" ]; then
  echo "error: -H is required (e.g. -H 172.24.0.60). The server certificate must" >&2
  echo "       list every address runners use, or TLS verification will fail." >&2
  exit 2
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Prefer an installed binary; otherwise run from source.
if command -v arcatum-ca >/dev/null 2>&1; then
  CA=(arcatum-ca)
else
  GO_BIN="$(command -v go || echo /usr/local/go/bin/go)"
  if [ ! -x "$GO_BIN" ]; then
    echo "error: neither arcatum-ca nor go found in PATH" >&2
    exit 1
  fi
  CA=("$GO_BIN" run ./cmd/arcatum-ca)
fi

echo "==> PKI directory: $PKI_DIR"

if [ -f "$PKI_DIR/ca.pem" ]; then
  echo "==> CA already exists, keeping it"
  # init would also create these; add whichever is missing.
  if [ ! -f "$PKI_DIR/dispatch-signing.key" ]; then
    echo "==> creating dispatch-signing keypair"
    "${CA[@]}" signing -dir "$PKI_DIR"
  fi
  if [ ! -f "$PKI_DIR/secrets-master.key" ]; then
    echo "==> creating secrets master key"
    "${CA[@]}" master-key -dir "$PKI_DIR"
  fi
else
  echo "==> creating CA, dispatch-signing keypair and secrets master key"
  "${CA[@]}" init -dir "$PKI_DIR"
fi

echo "==> issuing server certificate for: $HOSTS"
"${CA[@]}" server -dir "$PKI_DIR" -hosts "$HOSTS"

echo "==> issuing admin certificate: $ADMIN_NAME"
"${CA[@]}" admin -dir "$PKI_DIR" -name "$ADMIN_NAME"

for id in "${RUNNERS[@]}"; do
  echo "==> issuing runner certificate: $id"
  "${CA[@]}" runner -dir "$PKI_DIR" -id "$id"
done

cat <<EOF

Done. Files in $PKI_DIR:

  ca.pem                  CA certificate — needed by server AND every runner
  ca.key                  CA private key — keep this on the server only
  server.pem/.key         server certificate
  dispatch-signing.key    signs jobs (server only)
  dispatch-signing.pub    verifies jobs (copy to every runner)
  secrets-master.key      encrypts stored secrets (server only) — BACK THIS UP
  admin-$ADMIN_NAME.pem/.key    your client certificate for the API/web UI
  runner-<id>.pem/.key    one pair per runner

Next: point server.toml at ca.pem, server.pem/.key, dispatch-signing.key and
secrets-master.key; point each runner.toml at ca.pem, its runner-<id> pair and
dispatch-signing.pub.
See README section "Security (mTLS and job signing)".

Losing secrets-master.key makes every stored secret unreadable — keep a backup
somewhere other than the machine it protects.
EOF

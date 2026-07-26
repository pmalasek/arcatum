#!/usr/bin/env bash
# Demo script that produces output gradually, so the web UI's live tail has something
# to follow while the run is still in progress.
set -euo pipefail
for i in $(seq 1 6); do
  echo "krok $i z 6 — $(date +%H:%M:%S)"
  sleep 1
done
echo "hotovo"

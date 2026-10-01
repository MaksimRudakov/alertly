#!/usr/bin/env bash
# Stops what run-local.sh started.
set -euo pipefail
cd "$(dirname "$0")"
for name in alertly am-prod am-data; do
  pidfile=".rendered/${name}.pid"
  if [[ -f "${pidfile}" ]]; then
    kill "$(cat "${pidfile}")" 2>/dev/null || true
    rm -f "${pidfile}"
    echo "stopped ${name}" >&2
  fi
done

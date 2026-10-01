#!/usr/bin/env bash
# Fires (or resolves) a test alert in one of the stand's Alertmanagers.
#   ./send-alert.sh prod                 # firing DiskFull in k8s-prod
#   ./send-alert.sh data HighLatency warning
#   ./send-alert.sh prod DiskFull critical resolve
#   LONG=1 ./send-alert.sh prod           # ~6k chars description (message splitting)
set -euo pipefail

cluster="${1:-prod}"
name="${2:-DiskFull}"
severity="${3:-critical}"
action="${4:-fire}"

case "${cluster}" in
  prod) port=9093 ;;
  data) port=9094 ;;
  *) echo "cluster must be prod or data" >&2; exit 1 ;;
esac

description="Disk usage on node-1 is above 95% for 10 minutes."
if [[ "${LONG:-0}" == 1 ]]; then
  description="$(printf 'Line %s: something keeps happening on node-1, and here is a long explanation of it. ' $(seq 1 80))"
fi

ends_at=""
if [[ "${action}" == "resolve" ]]; then
  ends_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi

payload=$(python3 - "${name}" "${severity}" "${cluster}" "${description}" "${ends_at}" <<'PY'
import json, sys
name, severity, cluster, description, ends_at = sys.argv[1:6]
alert = {
    "labels": {"alertname": name, "severity": severity, "namespace": "db", "instance": "node-1", "cluster": cluster},
    "annotations": {
        "summary": f"{name} on node-1",
        "description": description,
        "runbook_url": "https://example.com/runbooks/" + name.lower() + "?q=\"x\"&y=1",
    },
}
if ends_at:
    alert["endsAt"] = ends_at
print(json.dumps([alert]))
PY
)

curl -fsS -X POST "http://127.0.0.1:${port}/api/v2/alerts" -H 'Content-Type: application/json' -d "${payload}"
echo "${action}: ${name} (${severity}) -> AM ${cluster} (:${port}); AM waits group_wait=5s before notifying" >&2

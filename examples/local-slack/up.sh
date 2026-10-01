#!/usr/bin/env bash
# Renders alertly.yaml from .env and starts the stand.
set -euo pipefail
cd "$(dirname "$0")"

if [[ ! -f .env ]]; then
  echo "no .env: cp .env.example .env and fill it in" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
source .env
set +a
for v in SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_CHANNEL_PROD SLACK_CHANNEL_DATA; do
  if [[ -z "${!v:-}" ]]; then
    echo "${v} is empty in .env" >&2
    exit 1
  fi
done

mkdir -p .rendered
envsubst '${SLACK_CHANNEL_PROD} ${SLACK_CHANNEL_DATA} ${SLACK_USER_ALLOWLIST}' \
  < alertly.yaml.tmpl > .rendered/alertly.yaml
docker compose up -d --build
echo "alertly:  http://127.0.0.1:8080/readyz" >&2
echo "AM prod:  http://127.0.0.1:9093   AM data: http://127.0.0.1:9094" >&2
echo "logs:     docker compose logs -f alertly" >&2

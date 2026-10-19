#!/usr/bin/env bash
# The stand without Docker: official Alertmanager binaries + alertly built
# from this checkout, all on 127.0.0.1. Logs and PIDs live in .rendered/.
set -euo pipefail
cd "$(dirname "$0")"

AM_VERSION="0.28.1"
AM_TARBALL="alertmanager-${AM_VERSION}.linux-amd64.tar.gz"
AM_URL="https://github.com/prometheus/alertmanager/releases/download/v${AM_VERSION}"
OUT=".rendered"

log() { echo "$*" >&2; }

if [[ ! -f .env ]]; then
  log "no .env: cp .env.example .env and fill it in"
  exit 1
fi
set -a
# shellcheck disable=SC1091
source .env
set +a
for v in SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_CHANNEL_PROD SLACK_CHANNEL_DATA; do
  if [[ -z "${!v:-}" ]]; then
    log "${v} is empty in .env"
    exit 1
  fi
done
mkdir -p "${OUT}"

if [[ ! -x "${OUT}/alertmanager" ]]; then
  log "downloading Alertmanager v${AM_VERSION}"
  curl -fsSL -o "${OUT}/${AM_TARBALL}" "${AM_URL}/${AM_TARBALL}"
  curl -fsSL -o "${OUT}/sha256sums.txt" "${AM_URL}/sha256sums.txt"
  (cd "${OUT}" && grep " ${AM_TARBALL}\$" sha256sums.txt | sha256sum -c -)
  tar -xzf "${OUT}/${AM_TARBALL}" -C "${OUT}" --strip-components=1 "alertmanager-${AM_VERSION}.linux-amd64/alertmanager"
fi

log "building alertly"
go build -o "${OUT}/alertly" ../../cmd/alertly

envsubst '${SLACK_CHANNEL_PROD} ${SLACK_CHANNEL_DATA} ${SLACK_USER_ALLOWLIST}' < alertly.yaml.tmpl \
  | sed -e 's#listen_addr: ":8080"#listen_addr: "127.0.0.1:8080"#' \
        -e 's#http://am-prod:9093#http://127.0.0.1:9093#' \
        -e 's#http://am-data:9093#http://127.0.0.1:9094#' > "${OUT}/alertly-local.yaml"

start() { # start <name> <cmd...>
  local name="$1"; shift
  "$@" > "${OUT}/${name}.log" 2>&1 &
  echo $! > "${OUT}/${name}.pid"
}

for c in prod data; do
  port=9093
  [[ "${c}" == data ]] && port=9094
  sed 's#http://alertly:8080#http://127.0.0.1:8080#' "alertmanager-${c}.yml" > "${OUT}/alertmanager-${c}.yml"
  mkdir -p "${OUT}/am-${c}-data"
  start "am-${c}" "${OUT}/alertmanager" \
    --config.file="${OUT}/alertmanager-${c}.yml" \
    --storage.path="${OUT}/am-${c}-data" \
    --web.listen-address="127.0.0.1:${port}" \
    --cluster.listen-address=""
done

WEBHOOK_AUTH_TOKEN_K8S_PROD=local-prod-token WEBHOOK_AUTH_TOKEN_K8S_DATA=local-data-token \
  ALERTLY_CONFIG="${OUT}/alertly-local.yaml" start alertly "${OUT}/alertly"

for _ in $(seq 1 30); do
  curl -sf http://127.0.0.1:8080/readyz >/dev/null && break
  sleep 0.5
done
curl -s http://127.0.0.1:8080/readyz >&2; echo >&2
log "alertly log: ${OUT}/alertly.log   AM: http://127.0.0.1:9093 (prod), http://127.0.0.1:9094 (data)"
log "stop: ./stop-local.sh"

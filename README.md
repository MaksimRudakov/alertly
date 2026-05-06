# alertly

[![CI](https://github.com/MaksimRudakov/alertly/actions/workflows/ci.yaml/badge.svg)](https://github.com/MaksimRudakov/alertly/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/MaksimRudakov/alertly?sort=semver)](https://github.com/MaksimRudakov/alertly/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/github/go-mod/go-version/MaksimRudakov/alertly)](go.mod)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/MaksimRudakov/alertly/badge)](https://securityscorecards.dev/viewer/?uri=github.com/MaksimRudakov/alertly)

Lightweight HTTP service that ingests webhooks from **Alertmanager** and **Kubewatch** and forwards them to **Telegram** chats. Stdlib-first, single static binary, distroless image.

## Features

- Sources: Alertmanager (v4 webhook) and Kubewatch (new + legacy payload).
- Multiple chats and topic threads per webhook URL: `/v1/alertmanager/-100123,-100456:42`.
- Per-chat + global Telegram rate limiter; retry with exponential backoff and `Retry-After` honoring.
- **Deadline-aware retry**: aborts the next backoff sleep when there's no time left to ACK the caller, preventing «delivered to Telegram but caller already gave up» duplicates.
- **In-process deduplication** by `(fingerprint, chat, status)` with TTL — suppresses duplicate Telegram messages caused by Alertmanager re-sending a webhook it didn't get an ACK for.
- Message splitting >4096 chars on rune boundaries, HTML-tag aware.
- `text/template` rendering with helpers (`severity_emoji`, `escape_html`, `truncate`, `join`, `humanize_duration`).
- Bearer-token webhook auth.
- Prometheus metrics, structured `slog` JSON logs, `/healthz` + `/readyz` (Telegram getMe + send-failure window).
- Multi-arch image (amd64, arm64), distroless static, ~10 MB, runs as UID 65532.

## Quick start (Docker)

```bash
docker run --rm -p 8080:8080 \
  -e TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN \
  -e WEBHOOK_AUTH_TOKEN=$WEBHOOK_AUTH_TOKEN \
  -e ALERTLY_CONFIG=/etc/alertly/config.yaml \
  -v $PWD/examples/config.yaml:/etc/alertly/config.yaml:ro \
  ghcr.io/maksimrudakov/alertly:latest
```

## Installation (Helm)

### Prerequisites

- Kubernetes 1.25+ (probes use `startupProbe`-compatible semantics).
- Helm 3.8+ (required for OCI install; any 3.x works for the HTTP repo).
- A Telegram bot token from [@BotFather](https://t.me/BotFather) and any random string for `WEBHOOK_AUTH_TOKEN` (at least 32 chars recommended).
- The bot added to the target chat(s); get the chat ID from [@RawDataBot](https://t.me/RawDataBot) or your own method.

### Add the chart repository

HTTP (GitHub Pages):

```bash
helm repo add alertly https://maksimrudakov.github.io/alertly
helm repo update
helm search repo alertly
```

OCI (GitHub Container Registry, no `helm repo add` needed):

```bash
helm show chart oci://ghcr.io/maksimrudakov/charts/alertly --version 0.2.0
```

### Install

Quick install with tokens passed directly (fine for a lab / personal cluster — **NOT for production**, tokens end up in Helm history):

```bash
helm install alertly alertly/alertly \
  --namespace monitoring-system --create-namespace \
  --version 0.2.0 \
  --set secret.values.telegramBotToken=<TOKEN> \
  --set secret.values.webhookAuthToken=<TOKEN>
```

Or from OCI:

```bash
helm install alertly oci://ghcr.io/maksimrudakov/charts/alertly \
  --namespace monitoring-system --create-namespace \
  --version 0.2.0 \
  --set secret.values.telegramBotToken=<TOKEN> \
  --set secret.values.webhookAuthToken=<TOKEN>
```

### Production install (external Secret)

Create a Secret out of band (external-secrets / sealed-secrets / vault / whatever you use) with the two expected keys:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: alertly-tokens
  namespace: monitoring-system
type: Opaque
stringData:
  TELEGRAM_BOT_TOKEN: "<TOKEN>"
  WEBHOOK_AUTH_TOKEN: "<TOKEN>"
```

Then install referencing it:

```bash
helm install alertly alertly/alertly \
  --namespace monitoring-system --create-namespace \
  --version 0.2.0 \
  --set secret.create=false \
  --set secret.existingSecret=alertly-tokens \
  --set reloader.enabled=true   # optional: auto-restart on Secret/ConfigMap changes
```

For a fully declarative setup pass a values file instead of `--set` flags — see [`charts/alertly/values.yaml`](./charts/alertly/values.yaml) for the full schema.

### Verify signatures

Both the chart tarball (attached to the GitHub Release) and the OCI chart manifest are **cosign-signed, keyless (Fulcio/Rekor)**. Verify either before installing in a high-trust environment:

```bash
# OCI manifest
cosign verify \
  --certificate-identity-regexp "https://github.com/MaksimRudakov/alertly/.github/workflows/release.yaml@.*" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/maksimrudakov/charts/alertly:0.2.0

# .tgz from GitHub Release (download the .tgz, .sig, .pem from the alertly-0.2.0 release)
cosign verify-blob \
  --certificate alertly-0.2.0.tgz.pem \
  --signature alertly-0.2.0.tgz.sig \
  --certificate-identity-regexp "https://github.com/MaksimRudakov/alertly/.github/workflows/release.yaml@.*" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  alertly-0.2.0.tgz
```

The container image `ghcr.io/maksimrudakov/alertly` is signed the same way.

### Upgrade

```bash
helm repo update
helm upgrade alertly alertly/alertly --namespace monitoring-system --version <new-version> --reuse-values
```

### Uninstall

```bash
helm uninstall alertly --namespace monitoring-system
```

The externally-managed Secret is not deleted (it was not created by the release).

### Values reference

Full list of values with defaults and descriptions: [`charts/alertly/README.md`](./charts/alertly/README.md) (auto-generated from `values.yaml`).

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/v1/alertmanager/{chats}` | Bearer | Alertmanager webhook |
| `POST` | `/v1/kubewatch/{chats}` | Bearer | Kubewatch webhook |
| `GET`  | `/healthz` | — | Liveness |
| `GET`  | `/readyz`  | — | Readiness (`getMe` ok + recent send health) |
| `GET`  | `/metrics` | — | Prometheus metrics |

`{chats}` accepts a comma-separated list of chat IDs with an optional thread:
`-1001234567890,-100456:42`. Auth: `Authorization: Bearer ${WEBHOOK_AUTH_TOKEN}`.

## Configuration

Path from `ALERTLY_CONFIG` (default `/etc/alertly/config.yaml`). See [examples/config.yaml](./examples/config.yaml).

| Env | Required | Purpose |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | Bot token used to call the Bot API |
| `WEBHOOK_AUTH_TOKEN` | yes | Bearer token clients must present |
| `ALERTLY_CONFIG` | no  | Path to config (default `/etc/alertly/config.yaml`) |
| `LOG_LEVEL` | no  | Override `logging.level` from config |
| `DRY_RUN`   | no  | When `true`, skip Telegram calls but log/meter |

Hot reload of config is intentionally **not** supported in-process — use [`stakater/Reloader`](https://github.com/stakater/Reloader) to roll the Deployment on ConfigMap/Secret change.

## Templates

Stored inline in YAML, parsed via `text/template`. Helper funcs: `severity_emoji`, `escape_html`, `truncate`, `join`, `humanize_duration`. A template named per source (`alertmanager`, `kubewatch`) is preferred; falls back to `default`.

## Deduplication

Telegram has no idempotency key, so any retry from upstream — most commonly Alertmanager re-sending a webhook because the previous response did not arrive in time — would be delivered as a fresh chat message. alertly absorbs that retry with a small in-process cache.

Key: `fingerprint | chat_id | thread_id | status` (so `firing` and `resolved` of the same alert are kept separate).

| Setting | Default | Notes |
|---|---|---|
| `dedup.enabled` | `true` | Set `false` to disable entirely. |
| `dedup.ttl` | `1h` | Window during which a repeat delivery is suppressed. |

Behaviour:

- **All parts delivered** → reservation is kept → next identical webhook within TTL is dropped, returns `204 No Content`, increments `alertly_dedup_skipped_total{source,chat_id,status}`.
- **All parts failed** → reservation is rolled back → caller's retry will be allowed through.
- **Partial success** (long message split into several parts, some sent, some failed) → reservation is **kept**, so the caller's retry doesn't double-deliver the parts that already landed in the chat.

### Scaling considerations

The cache is **per-process**. With multiple alertly replicas behind a `Service`, one webhook may land on pod-A and its retry on pod-B — different caches, no dedup. Trade-offs:

| Setup | When | Note |
|---|---|---|
| `replicaCount: 1` + PDB `maxUnavailable: 0` | **default recommendation** | alertly is stateless and lightweight; restart window is the only dedup blind-spot. |
| `replicaCount: N` + Ingress with consistent-hash on path | when an HA policy mandates >1 replica | e.g. nginx `nginx.ingress.kubernetes.io/upstream-hash-by: "$request_uri"` — same `/v1/.../{chats}` URL always goes to the same pod. |
| Shared cache (Redis / Valkey) | not implemented | Would only be worth it if the HA Alertmanager pair itself fans out the same webhook from both replicas. Kept out of scope until a real signal demands it. |

A pod restart re-opens the dedup window for all in-flight alerts — accepted trade-off.

## Comparison

| | alertly | viento-group/kubernetes-monitoring-telegram-bot | Botkube | Alertmanager `telegram_configs` |
|---|---|---|---|---|
| Language / runtime | Go static, ~10 MB | Kotlin/JVM, ~109 MB | Go, ~150 MB | bundled |
| Maintained | yes | dead since 2021 | yes | yes |
| Alertmanager source | yes | yes | yes (alerts via webhook) | native |
| Kubewatch source | yes | yes | n/a | n/a |
| Multiple chats per webhook URL | yes | yes | n/a | per-receiver |
| Topic threads | yes | no | n/a | yes |
| Message splitting >4096 | yes | no (truncates) | n/a | no (errors) |
| Inline buttons (Phase 2) | planned | no | yes | no |
| Per-chat rate limit | yes | no | n/a | no |
| Prometheus metrics | yes | no | yes | yes |

## Metrics

| Metric | Type | Labels |
|---|---|---|
| `alertly_notifications_received_total` | counter | `source`, `status_code` |
| `alertly_notifications_sent_total` | counter | `chat_id`, `status` |
| `alertly_telegram_api_duration_seconds` | histogram | — |
| `alertly_telegram_retries_total` | counter | `reason` |
| `alertly_telegram_rate_limited_total` | counter | `chat_id` |
| `alertly_template_render_errors_total` | counter | `template` |
| `alertly_message_split_total` | counter | — |
| `alertly_auth_failures_total` | counter | — |
| `alertly_source_parse_duration_seconds` | histogram | `source` |
| `alertly_dedup_skipped_total` | counter | `source`, `chat_id`, `status` |
| `alertly_build_info` | gauge | `version`, `commit`, `go_version` |

`alertly_telegram_retries_total` uses these `reason` values: `429`, `5xx`, `network`, and `deadline_skip` (retry aborted because the request context would expire before the next backoff completed).

## Troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| `401 Unauthorized` on every webhook | wrong/missing `Authorization: Bearer` header | check `WEBHOOK_AUTH_TOKEN` matches client config |
| `/readyz` stuck on 503 with `telegram getMe failed` | bot token invalid or egress blocked | verify token via `getMe` manually; check NetworkPolicy / firewall to `api.telegram.org:443` |
| Sends fail with `429 Too Many Requests` | upstream burst > rate limit | already retried with `Retry-After`; tune `telegram.rate_limit.global_per_sec` |
| Template render error in logs | bad `text/template` syntax in config | validate locally; `default` template must always exist |
| Long messages dropped silently | not split? always check `alertly_message_split_total` | verify `parse_mode` is `HTML` and template doesn't emit unbalanced tags |
| Same alert delivered to Telegram twice | multiple alertly replicas without sticky routing | run `replicaCount: 1`, or hash the request path to a pod (see [Deduplication › Scaling](#scaling-considerations)) |
| `alertly_telegram_retries_total{reason="deadline_skip"}` growing | server `WriteTimeout` shorter than worst-case retry budget | raise `server.write_timeout` or lower `telegram.retry.max_backoff`; check Telegram `Retry-After` headers in logs |

## Architecture

```mermaid
flowchart LR
  AM[Alertmanager] -->|webhook| H[/v1/alertmanager/]
  KW[Kubewatch] -->|webhook| H2[/v1/kubewatch/]
  H --> P[Source.Parse]
  H2 --> P
  P --> N[Notification]
  N --> R[Renderer text/template]
  R --> S[SplitMessage 4096]
  S --> D{dedup.Reserve<br/>fp|chat|status}
  D -- already seen --> SKIP[skip + metric]
  D -- new --> RL[per-chat + global rate limit]
  RL --> T[Telegram Bot API]
  T -. retry 429/5xx<br/>deadline-aware .-> T
  T -- all parts failed --> FG[dedup.Forget]
```

## Make targets

```
make build       # statically-linked binary in bin/
make test        # go test -race ./...
make test-cover  # coverage report
make lint        # golangci-lint or fallback
make docker      # multi-stage image -> alertly:dev
make run         # run with examples/config.yaml
```

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). Bugs and ideas → [Issues](https://github.com/MaksimRudakov/alertly/issues), questions → [Discussions](https://github.com/MaksimRudakov/alertly/discussions).

## License

MIT — see [LICENSE](./LICENSE).

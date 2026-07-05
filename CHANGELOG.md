# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Generic webhook source** (`POST /v1/generic/{chats}`): alertly's own JSON contract (`title`, `body`, `severity`, `status`, `fingerprint`, `labels`, `annotations`, `links`, `timestamp`; single object or array up to 100). Any tool that can POST JSON — GitLab CI, Jira automation, ArgoCD notifications webhook, scripts — gets dedup, splitting, threads and rate limiting. Fingerprint is content-hashed when absent, so sender retries are absorbed. README documents the contract with GitLab/ArgoCD/Jira examples.
- **↩️ Undo button**: after a silence is created the keyboard is replaced with a short-lived Undo button that deletes the silence via `DELETE /api/v2/silence/{id}`. Window is `updates.undo_window` (default `5m`, `0` disables, max `1h`); state is in-memory with the same strict expire-on-restart policy. Metrics: `alertly_silences_deleted_total{status}`, gauge `alertly_undo_tracker_entries`.
- **Configurable silence scope** `updates.silence_matchers`: empty (default) keeps the current behaviour — matchers from all labels, narrowest silence; listing labels (e.g. `[alertname, namespace]`) creates a broader silence covering sibling alerts. A press producing zero matchers is refused.

## [0.3.0] - 2026-07-05

Reliability, observability and supply-chain hardening pass. No config changes required; all new chart features are opt-in.

### Added
- **Continuous Telegram health probe**: readiness is now driven by a periodic `getMe` (every 60s after startup, 3 consecutive failures flip unready), so a Telegram outage is detected even with no webhook traffic. Network-level send errors now count toward the readiness failure window (429 still does not, deliberately).
- **Alertmanager client retry**: `GET /api/v2/alerts` and `POST /api/v2/silences` retry network errors, 429 and 5xx (3 attempts, linear backoff) — a transient AM blip no longer fails a button press.
- Metrics: `alertly_label_cache_lookups_total{result}` and on-scrape size gauges `alertly_dedup_cache_entries`, `alertly_button_tracker_entries`, `alertly_label_cache_entries`.
- Helm chart: native `podDisruptionBudget.{enabled,minAvailable,maxUnavailable}` and `metrics.serviceMonitor.*` (interval, scrapeTimeout, labels, relabelings) — both off by default, previously only available via `extraManifests`.
- CI: `govulncheck` job (+ `make vuln`), CodeQL workflow, OpenSSF Scorecard workflow, kind-based chart install test (builds the image, `helm install --wait`, probes `/healthz` + `/readyz`).

### Changed
- **Callback processing is time-bounded** (15s per `callback_query`): a stuck Alertmanager or Telegram call no longer stalls the updates poller for minutes; the user still receives an answer.
- **Graceful shutdown waits for background workers** (poller, sweepers, health probe) up to `server.shutdown_timeout`, so an in-flight silence completes its ack/edit.
- **Kubewatch fingerprint now includes the event message**: dedup only absorbs identical redeliveries instead of collapsing distinct events on the same object for the whole TTL.
- Silence buttons whose `callback_data` would exceed Telegram's 64-byte limit are skipped at build time instead of failing the whole `sendMessage`.
- `getUpdates` long polling reuses a dedicated keep-alive HTTP client instead of building one per poll.
- Supply chain: all GitHub Actions pinned to commit SHAs; Docker base images pinned by digest; builder synced to Go 1.26 (was downloading the toolchain at build time).
- Internal: `LabelCache` eviction is O(1) (`container/list`), duplicated error-unwrap helpers replaced with `errors.As`.

### Fixed
- README: comparison table claimed inline buttons were «planned» (shipped in 0.1.0), metrics table was missing the callbacks/silences/poll-errors metrics, prerequisites mentioned a non-existent `startupProbe`.

## [0.2.0] - 2026-05-06

Reliability hardening for the Alertmanager-retry duplicate problem: when alertly successfully delivered a message to Telegram but did not ACK the webhook in time, Alertmanager would retry and the same alert was posted twice. Two complementary changes close that path. Defaults are safe — existing deployments get the fix on upgrade with no config changes required.

### Added
- **In-process deduplication** (`internal/dedup`). TTL cache keyed by `(fingerprint, chat_id, thread_id, status)`; `firing` and `resolved` are kept independent. Atomic `Reserve` / `Forget`: a reservation is rolled back only when **every** part for a target failed, so a partially-delivered multi-part alert is never re-delivered. Configurable via `dedup.enabled` (default `true`) and `dedup.ttl` (default `1h`). Per-process cache — see scaling notes in README.
- **Deadline-aware retry** in `internal/telegram`. Before sleeping for the next exponential backoff, the client checks `ctx.Deadline()` (driven by `server.write_timeout`); if the remaining budget is less than `wait + 500ms`, the retry is aborted and the current error is returned immediately. Prevents the «delivered to Telegram on a late attempt while Alertmanager already gave up» failure mode.
- Metrics:
  - `alertly_dedup_skipped_total{source,chat_id,status}`
  - new `reason` label value `deadline_skip` on `alertly_telegram_retries_total`
- Helm chart: `config.dedup.{enabled,ttl}` exposed in `values.yaml`.

### Changed
- Helm chart `version 0.1.0 → 0.2.0`, `appVersion "0.1.0" → "0.2.0"`.
- `replicaCount` documentation in the chart now spells out the per-pod cache constraint and recommends single-replica + PDB or path-based consistent hashing on Ingress for HA setups.
- README rewritten: new **Deduplication** section with behaviour table and scaling considerations, metrics table extended, troubleshooting entries for duplicate deliveries and growing `deadline_skip`, mermaid diagram updated.

### Fixed
- Stale README references to chart `0.0.1` updated to current version (regression from the `v0.1.0` cut).

## [0.1.0] - 2026-04-24

Phase 2b from the roadmap: in-chat silence actions for Alertmanager alerts. Off by default; existing deployments are unaffected until `updates.enabled=true` is set.

### Added
- Interactive inline **silence buttons** on firing Alertmanager messages (`🔇 Silence 1h/4h/24h`, durations configurable). Single row, no URL buttons — Runbook link remains in message body.
- **Telegram long polling** worker (`getUpdates` with `allowed_updates=["callback_query"]`) — no inbound HTTPS endpoint or Ingress changes required.
- **Alertmanager client** (`internal/alertmanager`): `GET /api/v2/alerts` (resolve labels by fingerprint), `POST /api/v2/silences` (exact-match matchers from all alert labels). Supports basic auth (`ALERTMANAGER_AUTH_USERNAME`/`_PASSWORD`) and bearer (`ALERTMANAGER_AUTH_TOKEN`).
- **LabelCache** — TTL+FIFO fallback so callbacks work even when AM has already forgotten about a resolved alert.
- **ButtonTracker + sweeper** — messages expire after `updates.button_ttl` (validated 2h..48h, default 8h); sweeper strips inline keyboards once per minute; late or orphaned clicks rejected with `⏰ Silence window expired.`
- Config sections `updates.*` and `alertmanager.*` with safe defaults (full validation refuses bad durations, missing AM URL, out-of-range `button_ttl`).
- Metrics:
  - `alertly_callbacks_received_total{action,status}` (statuses: `ok | invalid | auth_failed | expired | am_error | not_found`)
  - `alertly_silences_created_total{status}`
  - `alertly_updates_poll_errors_total{reason}`

### Changed
- `telegram.Client.SendMessage` now returns `(message_id int64, err error)` — required so the tracker can key by `(chat_id, message_id)`. Callers updated; `DryRun` path returns `(0, nil)`.
- Helm chart `version 0.0.2 → 0.1.0`, `appVersion "0.0.3" → "0.1.0"`.
- Helm chart exposes `config.updates.*`, `config.alertmanager.*`, and optional `secret.values.alertmanager{Username,Password,Token}` (rendered only when set — no change for existing installs).

### Security
- Silence actions gated by `updates.chat_allowlist` (empty ⇒ disabled) plus optional `user_allowlist`. Chat membership alone is **not** enough if `user_allowlist` is set.
- Tracker entries are in-memory; after a restart older messages' buttons reject clicks rather than silencing with guessed labels.

## [0.0.3] - 2026-04-21

Runtime behaviour unchanged vs `v0.0.2`. This release bumps the image tag to keep chart `appVersion` aligned with the code state after the following doc/test/example work.

### Added
- `examples/alertmanager-config.yaml` — Alertmanager receiver config with Bearer-token auth, multi-chat URL and per-chat forum-thread (`:thread_id`) routing.
- `examples/kubewatch-config.yaml` — robusta-dev/kubewatch ConfigMap + Deployment pointing at alertly with an `Authorization: Bearer` header.
- `examples/values-production.yaml` — production values for the Helm chart: external-secret (`secret.create=false`), Reloader, topology spread, PDB/PodMonitor/NetworkPolicy rendered via `extraManifests`.
- `testdata/alertmanager_long.json` — fixture with a >10 KB description to exercise the 4096-byte splitter end-to-end; verified locally that one notification produces 3 Telegram messages and increments `alertly_message_split_total`.

### Tests
- `internal/server` coverage raised from **62.7% → 90.8%**: new unit tests for `ReadinessTracker` (`MarkReady` / `MarkUnready` / `RecordSendFailure` — including the 10-failure window, client-error ignore path, and success-reset), handler multi-status (`207`) and all-failed (`500`) branches, `400` paths (invalid chat list, source parse error), `isServerError` table test (nil / non-api / 4xx / 429 / 5xx), `recoverMiddleware` panic path, and `Server.Run` graceful shutdown on ctx cancel.
- `alertly_source_parse_duration_seconds` histogram — new test asserts the sample count grows by 1 on both the happy path and the parse-error path, that the sum is positive, and that the `source` label does not leak between sources.

### Changed
- Helm chart bumped to `version: 0.0.2`, `appVersion: "0.0.3"`.
- `release.yaml`: dropped the standalone `anchore/sbom-action` step. SBOM is already produced by `docker/build-push-action` (`sbom: true`) and attached to the image as an OCI attestation, so the extra step was redundant and was failing on syft exit code 1.

## [0.0.1] - 2026-04-21

### Added
- Initial project scaffolding: Alertmanager + Kubewatch sources, Telegram client with retry/rate-limit/splitting, `text/template` renderer, Prometheus metrics, `/healthz` + `/readyz`.
- OSS scaffolding: CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, dependabot, issue/PR templates.
- CI workflow (lint, test, build, trivy fs/image scan, chart-lint with `helm lint` + `helm template` + `helm-docs` check).
- Release workflow (goreleaser binaries, multi-arch container image, cosign keyless signing, SBOM attestation).
- Helm chart `charts/alertly` (version 0.0.1, appVersion 0.0.1): Deployment/Service/ConfigMap/Secret/ServiceAccount/Ingress (opt-in) + `extraManifests` escape hatch for PodMonitor/PDB/NetworkPolicy. Published to GitHub Pages (`helm repo add`) and OCI (`oci://ghcr.io/maksimrudakov/charts`). Both tarball and OCI manifest cosign-signed.
- New alertmanager template: `Alert Name`, `Severity`, `Runbook URL` formatting; `generatorURL` is no longer emitted.

[Unreleased]: https://github.com/MaksimRudakov/alertly/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/MaksimRudakov/alertly/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/MaksimRudakov/alertly/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/MaksimRudakov/alertly/compare/v0.0.3...v0.1.0
[0.0.3]: https://github.com/MaksimRudakov/alertly/compare/v0.0.1...v0.0.3
[0.0.1]: https://github.com/MaksimRudakov/alertly/releases/tag/v0.0.1

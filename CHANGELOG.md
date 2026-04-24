# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/MaksimRudakov/alertly/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MaksimRudakov/alertly/compare/v0.0.3...v0.1.0
[0.0.3]: https://github.com/MaksimRudakov/alertly/compare/v0.0.1...v0.0.3
[0.0.1]: https://github.com/MaksimRudakov/alertly/releases/tag/v0.0.1

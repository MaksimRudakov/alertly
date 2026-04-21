# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`alertly` — Go HTTP service that ingests webhooks from Alertmanager and Kubewatch and forwards them to Telegram chats. Stdlib-first (`net/http` with `mux.Handle("POST /v1/...{chats}")`), no web framework.

- Module: `github.com/MaksimRudakov/alertly`
- Go: `1.25` (toolchain `go1.26.2`)
- Single binary entry: `cmd/alertly`

README references a `SPEC.md` that does not exist in the tree — treat README + `examples/config.yaml` + `TODO.md` as the source of truth for current scope. `TODO.md` lists Phase 2 items (dedup, inline buttons, generic source, label routing, async queue, Markdown, two-way chat) that are intentionally out of MVP.

## Common commands

```
make build         # static binary -> bin/alertly (CGO_ENABLED=0, -trimpath, ldflags inject version/commit/date)
make test          # go test -race -count=1 ./...
make test-cover    # writes coverage.out + coverage.html, prints total coverage
make lint          # golangci-lint run; falls back to go vet (+ staticcheck if installed)
make fmt           # gofmt -w . && goimports -w . (if installed)
make run           # ALERTLY_CONFIG=examples/config.yaml go run ./cmd/alertly
make docker        # builds alertly:dev (distroless/static-debian12:nonroot, UID 65532)
make tidy          # go mod tidy
```

Running a single test:

```
go test -race -run TestSplitMessage ./internal/telegram
go test -race -run TestParseChatTargets/with_thread ./internal/server
```

`make run` requires `TELEGRAM_BOT_TOKEN` and `WEBHOOK_AUTH_TOKEN` in env. Set `DRY_RUN=true` to skip actual Telegram calls (still logs/meters); the readiness probe is force-flipped to ready in dry-run.

Lint config (`.golangci.yaml`) is intentionally minimal: `gofmt`, `goimports`, `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`, `gosec` (G101/G304 enabled, G104 excluded), `revive` with `exported`/`package-comments` disabled. Tests are exempt from `gosec`/`errcheck`. Per `TODO.md`, `staticcheck` currently SIGSEGVs on Go 1.26 — the Makefile fallback path (`go vet`) is the working option until upstream releases a compatible build.

## Architecture

### Request lifecycle

`cmd/alertly/main.go` wires everything and starts `internal/server`. Per webhook:

1. `recoverMiddleware` → `requestIDMiddleware` (sets `X-Request-Id`) → `loggingMiddleware` (slog logger attached to ctx).
2. `authMiddleware` validates `Authorization: Bearer <WEBHOOK_AUTH_TOKEN>` with `subtle.ConstantTimeCompare`.
3. `webhookHandler` (handlers.go):
   - parses `{chats}` path value via `parseChatTargets` — comma-separated `chat_id[:thread_id]` (e.g. `-1001234567890,-100456:42`).
   - reads body through `http.MaxBytesReader(cfg.MaxBodyBytes)`.
   - calls `source.Parse(body)` → `[]notification.Notification`.
   - per notification: `renderer.Render(templateName, n)` → `telegram.SplitMessage(text, TelegramTextLimit=4096)` → `tg.SendMessage` per `(target, part)`.
   - aggregates outcome: `200` all-ok, `204` nothing sent, `207` partial, `500` all-failed; emits `alertly_notifications_received_total{source,status_code}` and `alertly_notifications_sent_total{chat_id,status}`.

### Source plugins (`internal/source`)

`Source` is `Name() + Parse([]byte) → []notification.Notification`. Implementations register themselves in `cmd/alertly/main.go` into a `map[string]Source`; the route `POST /v1/{name}/{chats}` is generated from that map. Adding a new source = new file in `internal/source` + entry in the map. The `templateName` passed to the handler equals the source name (with fallback to `default` inside the renderer).

`alertmanager`: standard webhook payload; severity from `labels.severity` (default `info`); title prefers `annotations.summary`, body prefers `annotations.description`; emits a Runbook link from `annotations.runbook_url` when present (Prometheus `generatorURL` is intentionally ignored — internal Prometheus links are rarely reachable from Telegram and add noise).

`kubewatch`: tries new (`eventmeta`) format then legacy flat; severity becomes `warning` for `Type=Warning` or `Reason=Failed`; fingerprint is sha256-16 of `kind|namespace|name|reason|type`.

### Notification → Telegram

`internal/notification.Notification` is the canonical struct — sources produce it, templates consume it. Templates are `text/template` (NOT `html/template`; HTML escaping is the caller's responsibility via `escape_html`). Parse mode is `HTML` (default; Markdown is Phase 2). Helpers: `severity_emoji`, `escape_html`, `truncate`, `join`, `humanize_duration`. A template named after the source is preferred; fallback is `default` (which is required to exist — config loader injects one if missing).

Splitter (`telegram/splitter.go`) cuts on rune boundaries at `\n\n`, `\n`, then space, then hard cut, and `avoidTagSplit` rewinds before any unclosed `<` to keep HTML tags intact. Increments `alertly_message_split_total` when split occurs.

`telegram.client` does retry with exponential backoff (`MaxAttempts`, `InitialBackoff`, `MaxBackoff`). `IsRetryable`: `429` and `5xx` are retryable; `4xx` is not; non-API errors (network) retryable. On `429`, honors `parameters.retry_after` from body or `Retry-After` header (capped to `MaxBackoff`). Rate limiter (`golang.org/x/time/rate`) enforces global + per-chat caps; `Wait` blocks before each send.

### Readiness model

`server/readyz.go` `ReadinessTracker`:
- starts unready with reason `startup: telegram getMe pending`;
- `startupCheck` in `main.go` calls `tg.GetMe` with exponential backoff and flips to ready on success;
- runtime: `RecordSendSuccess` resets a failure counter and re-arms ready; `RecordSendFailure(serverError=true)` increments and flips to unready after `readyzFailureWindow=10` consecutive 5xx;
- 4xx send failures do NOT degrade readiness (they're treated as caller/data problems).

`/healthz` is unconditional 200; `/readyz` returns 503 with JSON reason when not ready.

### Configuration

`config.Load(path)` starts from `config.Default()` and overlays YAML. Defaults are intentionally production-sane; `examples/config.yaml` is what `make run` uses. `LOG_LEVEL` env overrides `logging.level` post-parse. `Validate()` enforces non-zero timeouts/limits and valid log level/format. Hot-reload is **not** supported — restart the process.

Required env: `TELEGRAM_BOT_TOKEN`, `WEBHOOK_AUTH_TOKEN`. Optional: `ALERTLY_CONFIG` (default `/etc/alertly/config.yaml`), `LOG_LEVEL`, `DRY_RUN`.

### Metrics

All in `internal/metrics`. Custom registry (no default Go process collectors except those explicitly registered: `NewGoCollector`, `NewProcessCollector`). `Init()` is idempotent via `sync.Once`. Names: `alertly_notifications_{received,sent}_total`, `alertly_telegram_{api_duration_seconds,retries_total,rate_limited_total}`, `alertly_template_render_errors_total`, `alertly_message_split_total`, `alertly_auth_failures_total`, `alertly_source_parse_duration_seconds`, `alertly_build_info`.

## Repo conventions

- `internal/` for everything; `pkg/` exists but is empty — keep it that way unless something needs to be importable by external modules.
- Sources/templates/sinks are wired in `cmd/alertly/main.go` — the rest of the code is plain interfaces. When adding a webhook source, also add a same-named template entry in `examples/config.yaml`.
- Errors: wrap with `fmt.Errorf("%w", err)`; `telegram.APIError` carries `StatusCode` + optional `RetryAfter` and is the typed error the handler/readiness logic introspects.
- Logging: `slog` with JSON handler by default; per-request logger attached to `ctx` via `loggerFrom(ctx)` — handlers should use it, not `slog.Default()`.
- Build metadata is injected via ldflags into `internal/version` (`Version`, `Commit`, `Date`) — both Makefile and Dockerfile pass these.

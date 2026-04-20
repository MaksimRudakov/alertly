# Contributing

Thanks for your interest in alertly. Bug reports, feature ideas, and PRs are all welcome.

## Quick start

```bash
git clone https://github.com/MaksimRudakov/alertly.git
cd alertly
make test
```

## Local development

```bash
export TELEGRAM_BOT_TOKEN=<your-test-bot-token>
export WEBHOOK_AUTH_TOKEN=anything
export DRY_RUN=true       # skip Telegram calls but log/meter
make run
```

With `DRY_RUN=true` you can hit the endpoints without owning a chat:

```bash
curl -H "Authorization: Bearer anything" -X POST \
  http://localhost:8080/v1/alertmanager/-100123 \
  --data @testdata/alertmanager_firing.json
```

## Code style

- `make fmt` (`gofmt` + `goimports`).
- `make lint` (`golangci-lint`).
- `make test` must pass with `-race`.
- Keep test coverage of core logic (`internal/telegram`, `internal/template`, `internal/source`) ≥80%.
- Stdlib-first: avoid heavy frameworks (Gin/Echo/Fiber, ORMs).
- No comments on what code does — only why, and only when non-obvious.

## Commit style

[Conventional Commits](https://www.conventionalcommits.org/) — `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`. Used for changelog generation.

## Pull requests

- Branch off `main`, one logical change per PR.
- Update `CHANGELOG.md` under `## [Unreleased]`.
- Add or update tests for behavior changes.
- CI must be green before review.

## Releasing (maintainers)

1. Bump `version` / `appVersion` in `charts/alertly/Chart.yaml` (when chart exists).
2. Move `## [Unreleased]` section in `CHANGELOG.md` to a new `## [vX.Y.Z]` section.
3. Tag: `git tag -s vX.Y.Z -m "vX.Y.Z" && git push --tags`.
4. The release workflow builds binaries, multi-arch image, signs with cosign, attaches SBOM, and publishes the chart.

## Reporting security issues

See [SECURITY.md](./SECURITY.md). Do **not** open public issues for vulnerabilities.

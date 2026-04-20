# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- `release.yaml`: dropped the standalone `anchore/sbom-action` step. SBOM is already produced by `docker/build-push-action` (`sbom: true`) and attached to the image as an OCI attestation, so the extra step was redundant and was failing on syft exit code 1.

## [0.0.1] - 2026-04-21

### Added
- Initial project scaffolding: Alertmanager + Kubewatch sources, Telegram client with retry/rate-limit/splitting, `text/template` renderer, Prometheus metrics, `/healthz` + `/readyz`.
- OSS scaffolding: CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, dependabot, issue/PR templates.
- CI workflow (lint, test, build, trivy fs/image scan).
- Release workflow (goreleaser binaries, multi-arch container image `ghcr.io/maksimrudakov/alertly`, cosign keyless signing, SBOM attestation via build-push-action).

[Unreleased]: https://github.com/MaksimRudakov/alertly/compare/v0.0.1...HEAD
[0.0.1]: https://github.com/MaksimRudakov/alertly/releases/tag/v0.0.1

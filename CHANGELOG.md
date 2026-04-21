# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Initial project scaffolding: Alertmanager + Kubewatch sources, Telegram client with retry/rate-limit/splitting, `text/template` renderer, Prometheus metrics, `/healthz` + `/readyz`.
- OSS scaffolding: CONTRIBUTING, SECURITY, CODE_OF_CONDUCT, dependabot, issue/PR templates.
- CI workflow (lint, test, build, trivy fs/image scan, chart-lint with `helm lint` + `helm template` + `helm-docs` check).
- Release workflow (goreleaser binaries, multi-arch container image, cosign keyless signing, SBOM attestation).
- Helm chart `charts/alertly` (version 0.0.1, appVersion 0.0.1): Deployment/Service/ConfigMap/Secret/ServiceAccount/Ingress (opt-in) + `extraManifests` escape hatch for PodMonitor/PDB/NetworkPolicy. Published to GitHub Pages (`helm repo add`) and OCI (`oci://ghcr.io/maksimrudakov/charts`). Both tarball and OCI manifest cosign-signed.
- New alertmanager template: `Alert Name`, `Severity`, `Runbook URL` formatting; `generatorURL` is no longer emitted.

[Unreleased]: https://github.com/MaksimRudakov/alertly/compare/HEAD

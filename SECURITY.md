# Security Policy

## Supported versions

Only the latest minor release receives security fixes. Once `v1.0.0` ships, the previous minor will receive critical fixes for 90 days.

## Reporting a vulnerability

**Please do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

Use GitHub's private vulnerability reporting:

1. Go to https://github.com/MaksimRudakov/alertly/security/advisories/new
2. Provide a clear description, reproduction steps, and impact assessment.
3. We will acknowledge within 72 hours and aim to ship a fix within 14 days for HIGH/CRITICAL issues.

## Scope

In scope:
- The alertly binary and its handling of webhook payloads, auth tokens, and Telegram API interactions.
- The Helm chart (when published).
- The container image published to `ghcr.io/maksimrudakov/alertly`.

Out of scope:
- Vulnerabilities in dependencies that have not yet been patched upstream — please report those to the dependency maintainers; we track and update via Dependabot.
- Misconfiguration on the operator's side (exposed bot token, weak `WEBHOOK_AUTH_TOKEN`, public Service without auth, etc.).

## Disclosure

We follow [coordinated disclosure](https://en.wikipedia.org/wiki/Coordinated_vulnerability_disclosure). After a fix is released, we publish a GitHub Security Advisory with credit to the reporter (unless anonymity is requested).

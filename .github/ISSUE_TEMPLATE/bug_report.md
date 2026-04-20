---
name: Bug report
about: Report a problem you can reproduce
title: "bug: <one-line summary>"
labels: [bug]
---

**Symptoms**
What happens. What did you expect instead?

**Reproduction**
1. Config snippet (redact tokens)
2. Webhook payload (or curl command)
3. Observed response / log line

**Environment**
- alertly version (image tag or `git describe`):
- Deployment: docker / helm / source
- Kubernetes version (if applicable):
- Telegram chat type: private / group / supergroup with topic

**Logs / metrics**
```
<paste relevant slog lines, redact bot token / chat IDs if needed>
```

**Anything else?**

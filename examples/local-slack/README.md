# Local Slack test stand

Two Alertmanagers (clusters `k8s-prod`, `k8s-data`) and alertly built from
this checkout, posting to a **real** Slack workspace. Interactivity runs over
Socket Mode — an outbound WebSocket — so nothing has to be reachable from the
internet. Telegram is off here on purpose: polling a shared bot token from a
laptop would steal button presses from the instance that already uses it (409).

```
send-alert.sh ─► Alertmanager prod/data ─webhook─► alertly ─► Slack
                        ▲                            │
                        └── silence / undo (AM API) ◄┘◄── button presses (Socket Mode)
```

## 1. Slack App (once)

1. Use a test workspace (a free one works).
2. api.slack.com/apps → **Create New App → From an app manifest** → paste
   [`../slack-app-manifest.yaml`](../slack-app-manifest.yaml) → **Install to Workspace**.
3. Tokens — into `.env`, never into chat or git:
   - **OAuth & Permissions → Bot User OAuth Token** (`xoxb-…`) → `SLACK_BOT_TOKEN`;
   - **Basic Information → App-Level Tokens → Generate Token and Scopes**,
     scope `connections:write` (`xapp-…`) → `SLACK_APP_TOKEN`.
   Client Secret, Signing Secret and Verification Token are **not** used
   (Socket Mode needs neither request signing nor OAuth exchange).
4. Two channels, e.g. `#alerts-prod`, `#alerts-data`; in each: `/invite @alertly`.
   Channel ID: click the channel name → bottom of *About* → `C…`.

## 2. Run

Docker Desktop on WSL: *Settings → Resources → WSL integration* must be on
for this distro (`docker compose version` must work).

```bash
cp .env.example .env      # fill in tokens and channel IDs
./up.sh                   # renders .rendered/alertly.yaml, builds alertly, starts the stand
docker compose logs -f alertly
```

`curl -s 127.0.0.1:8080/readyz` → `"slack":{"ready":true}`; the log shows
`slack socket: connected`.

## 3. Checklist

| # | Do | Expect |
|---|---|---|
| 1 | `./send-alert.sh prod` | ~5 s later in the prod channel: `🔥 DiskFull on node-1`, `Firing · cluster k8s-prod · critical`, fields alertname/namespace/instance, Runbook link, buttons 🔇 1h / 4h / 24h |
| 2 | `./send-alert.sh data HighLatency warning` | data channel only, `⚠️` in the header |
| 3 | Press **🔇 Silence 1h** in prod | ephemeral "Silenced 1h until …"; buttons replaced by **↩️ Undo silence**; silence in AM prod (http://127.0.0.1:9093/#/silences, created by `slack:@you`), none in AM data (:9094) |
| 4 | Press **↩️ Undo silence** | ephemeral "Silence removed"; buttons gone; silence expired in AM prod |
| 5 | `./send-alert.sh prod DiskFull critical resolve` | `Resolved` in the status line, no buttons |
| 6 | `/alertly status` in the prod channel | in-channel reply: version, `Slack: ✅`, `Pipeline — k8s-prod` (AM v0.28.1, alert counts); no k8s-data |
| 7 | `/alertly status data` in the prod channel | ephemeral "Unknown cluster data" (not routed to this channel) |
| 8 | `/alertly status` in the data channel | `Pipeline — k8s-data` only |
| 9 | `/alertly help` | ephemeral usage |
| 10 | `LONG=1 ./send-alert.sh prod Verbose info` | long description split into sections / continuation messages, nothing cut mid-word |
| 11 | `docker compose restart alertly`, then press an old silence button | log `slack socket: connected` again; press answers "Silence window expired", buttons removed |

Optional: put your Slack user ID into `SLACK_USER_ALLOWLIST`, re-run
`./up.sh`, and press a button from another account — it must be refused.

## 4. Clean up

```bash
docker compose down -v
rm -rf .rendered
```

Revoke the tokens (or delete the app) when done if the workspace is shared.

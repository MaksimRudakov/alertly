package server

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
)

// SizeStat exposes the current size of one in-memory cache for /status output.
type SizeStat struct {
	Label string
	Len   func() int
}

// PipelineConfig controls the Alertmanager/Prometheus section of /status.
type PipelineConfig struct {
	Enabled bool
	// WatchdogAlert is the always-firing deadman's-switch alert (Watchdog in
	// kube-prometheus-stack). Present and fresh in AM = the Prometheus → AM
	// pipeline is alive; empty disables the check.
	WatchdogAlert string
	// Timeout bounds each AM call so a dead AM delays the reply instead of
	// eating the whole callback handle budget.
	Timeout time.Duration
}

// watchdogStaleAfter flags a Watchdog that AM still holds but Prometheus has
// stopped refreshing. Refresh cadence is the rule evaluation interval (~30s
// default); 15m of no updates means evaluation stalled long ago.
const watchdogStaleAfter = 15 * time.Minute

// StatusReporter renders the /status chat-ops reply: build info, uptime,
// readiness, delivery-pipeline health and in-memory cache sizes. All fields
// are wired once at startup; Text is safe for concurrent use as long as Len
// funcs are.
type StatusReporter struct {
	StartedAt time.Time
	Version   string
	Commit    string
	Readiness ReadinessTracker
	Sizes     []SizeStat

	// AM + Pipeline enable the pipeline section; Activity adds last-seen
	// webhook/delivery lines. All optional.
	AM       alertmanager.Client
	Pipeline PipelineConfig
	Activity *ActivityTracker
}

func (r *StatusReporter) Text(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("🩺 <b>alertly status</b>\n")
	fmt.Fprintf(&b, "Version: %s (%s)\n", html.EscapeString(r.Version), html.EscapeString(shortCommit(r.Commit)))
	fmt.Fprintf(&b, "Uptime: %s\n", time.Since(r.StartedAt).Truncate(time.Second))

	ready, reason := r.Readiness.IsReady()
	if ready {
		b.WriteString("Ready: ✅\n")
	} else {
		fmt.Fprintf(&b, "Ready: ❌ %s\n", html.EscapeString(reason))
	}
	if last := r.Readiness.LastCheck(); !last.IsZero() {
		fmt.Fprintf(&b, "Last Telegram check: %s ago\n", time.Since(last).Truncate(time.Second))
	}

	r.writePipeline(ctx, &b)

	for _, s := range r.Sizes {
		fmt.Fprintf(&b, "%s: %d\n", html.EscapeString(s.Label), s.Len())
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *StatusReporter) writePipeline(ctx context.Context, b *strings.Builder) {
	if !r.Pipeline.Enabled || r.AM == nil {
		return
	}
	timeout := r.Pipeline.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}

	b.WriteString("\n<b>Pipeline</b>\n")

	sctx, cancel := context.WithTimeout(ctx, timeout)
	st, err := r.AM.Status(sctx)
	cancel()
	if err != nil {
		fmt.Fprintf(b, "Alertmanager: ❌ unreachable — %s\n", html.EscapeString(compactError(err)))
		b.WriteString("Watchdog/alerts: skipped (AM unreachable)\n")
	} else {
		cluster := st.ClusterStatus
		if cluster == "" {
			cluster = "n/a"
		}
		fmt.Fprintf(b, "Alertmanager: ✅ v%s, cluster %s\n", html.EscapeString(st.Version), html.EscapeString(cluster))

		actx, cancel := context.WithTimeout(ctx, timeout)
		ov, err := r.AM.AlertsOverview(actx, r.Pipeline.WatchdogAlert)
		cancel()
		if err != nil {
			fmt.Fprintf(b, "Alerts: ⚠️ query failed — %s\n", html.EscapeString(compactError(err)))
		} else {
			if r.Pipeline.WatchdogAlert != "" {
				switch {
				case !ov.WatchdogSeen:
					fmt.Fprintf(b, "Watchdog: ❌ %q not in AM — check Prometheus / rule evaluation\n", r.Pipeline.WatchdogAlert)
				case time.Since(ov.WatchdogUpdatedAt) > watchdogStaleAfter:
					fmt.Fprintf(b, "Watchdog: ⚠️ stale, updated %s ago — Prometheus may be down\n",
						time.Since(ov.WatchdogUpdatedAt).Truncate(time.Second))
				default:
					fmt.Fprintf(b, "Watchdog: ✅ updated %s ago\n", time.Since(ov.WatchdogUpdatedAt).Truncate(time.Second))
				}
			}
			fmt.Fprintf(b, "Alerts in AM: %d firing, %d silenced\n", ov.Firing, ov.Silenced)
		}
	}

	if r.Activity != nil {
		if t, src := r.Activity.LastWebhook(); t.IsZero() {
			b.WriteString("Last webhook: none since start\n")
		} else {
			fmt.Fprintf(b, "Last webhook: %s ago (%s)\n", time.Since(t).Truncate(time.Second), html.EscapeString(src))
		}
		if t := r.Activity.LastDelivery(); t.IsZero() {
			b.WriteString("Last delivery: none since start\n")
		} else {
			fmt.Fprintf(b, "Last delivery: %s ago\n", time.Since(t).Truncate(time.Second))
		}
	}
	b.WriteString("\n")
}

func shortCommit(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// compactError keeps AM failure messages chat-sized: one line, no nested URLs.
func compactError(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

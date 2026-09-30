package server

import (
	"context"
	"fmt"
	"html"
	"strings"
	"sync"
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
	// pipeline is alive; empty disables the check. Single-cluster mode only;
	// clusters carry their own.
	WatchdogAlert string
	// Timeout bounds each AM call so a dead AM delays the reply instead of
	// eating the whole callback handle budget.
	Timeout time.Duration
}

// watchdogStaleAfter flags a Watchdog that AM still holds but Prometheus has
// stopped refreshing. Refresh cadence is the rule evaluation interval (~30s
// default); 15m of no updates means evaluation stalled long ago.
const watchdogStaleAfter = 15 * time.Minute

// StatusReporter builds the /status reply: build info, uptime, readiness,
// delivery-pipeline health per cluster and in-memory cache sizes. All fields
// are wired once at startup; it is safe for concurrent use as long as the
// Len funcs are.
type StatusReporter struct {
	StartedAt time.Time
	Version   string
	Commit    string
	// Readiness is reported when Sinks is nil (single-sink mode).
	Readiness ReadinessTracker
	// Sinks reports readiness per messenger.
	Sinks *SinkReadiness
	Sizes []SizeStat

	// AM + Pipeline describe the single-cluster pipeline; with Clusters set
	// each cluster's own AM and Watchdog are used instead (Pipeline still
	// gates the section and sets the timeout). Activity adds last-seen
	// webhook/delivery lines. All optional.
	AM       alertmanager.Client
	Pipeline PipelineConfig
	Clusters []*Cluster
	Activity *ActivityTracker
}

// StatusSnapshot is the messenger-neutral content of a /status reply.
type StatusSnapshot struct {
	Version   string
	Commit    string
	Uptime    time.Duration
	Ready     bool
	Reason    string
	LastCheck time.Time
	// Sinks is set when more than one messenger is enabled.
	Sinks     []SinkState
	Pipelines []PipelineStatus
	Activity  *ActivitySnapshot
	Sizes     []SizeValue
}

// PipelineStatus is the Alertmanager/Prometheus health of one cluster.
type PipelineStatus struct {
	Cluster       string // "" in single-cluster mode
	AMError       string
	AMVersion     string
	AMCluster     string
	AlertsError   string
	Firing        int
	Silenced      int
	WatchdogAlert string // "" = check disabled
	WatchdogSeen  bool
	WatchdogAge   time.Duration
}

type ActivitySnapshot struct {
	LastWebhook   time.Time
	WebhookSource string
	LastDelivery  time.Time
}

type SizeValue struct {
	Label string
	N     int
}

// Text renders the single-view Telegram reply (all clusters of the reporter).
func (r *StatusReporter) Text(ctx context.Context) string {
	return FormatStatusTelegram(r.Snapshot(ctx, nil))
}

// Snapshot collects the status. clusters selects which clusters' pipelines
// to show; nil = the reporter's own (single-cluster AM, or all Clusters).
func (r *StatusReporter) Snapshot(ctx context.Context, clusters []*Cluster) StatusSnapshot {
	s := StatusSnapshot{
		Version: r.Version,
		Commit:  shortCommit(r.Commit),
		Uptime:  time.Since(r.StartedAt).Truncate(time.Second),
	}
	switch {
	case r.Sinks != nil:
		s.Ready, s.Reason = r.Sinks.IsReady()
		states := r.Sinks.States()
		for _, st := range states {
			if st.LastCheck.After(s.LastCheck) {
				s.LastCheck = st.LastCheck
			}
		}
		if len(states) > 1 {
			s.Sinks = states
		}
	case r.Readiness != nil:
		s.Ready, s.Reason = r.Readiness.IsReady()
		s.LastCheck = r.Readiness.LastCheck()
	}

	if r.Pipeline.Enabled {
		s.Pipelines = r.pipelines(ctx, clusters)
		if len(s.Pipelines) > 0 && r.Activity != nil {
			a := &ActivitySnapshot{}
			a.LastWebhook, a.WebhookSource = r.Activity.LastWebhook()
			a.LastDelivery = r.Activity.LastDelivery()
			s.Activity = a
		}
	}
	for _, sz := range r.Sizes {
		s.Sizes = append(s.Sizes, SizeValue{Label: sz.Label, N: sz.Len()})
	}
	return s
}

type pipelineTarget struct {
	name     string
	am       alertmanager.Client
	watchdog string
}

func (r *StatusReporter) pipelines(ctx context.Context, clusters []*Cluster) []PipelineStatus {
	var targets []pipelineTarget
	switch {
	case clusters != nil:
		for _, c := range clusters {
			if c.AM != nil {
				targets = append(targets, pipelineTarget{c.DisplayName(), c.AM, c.WatchdogAlert})
			}
		}
	case len(r.Clusters) > 0:
		for _, c := range r.Clusters {
			if c.AM != nil {
				targets = append(targets, pipelineTarget{c.DisplayName(), c.AM, c.WatchdogAlert})
			}
		}
	case r.AM != nil:
		targets = append(targets, pipelineTarget{"", r.AM, r.Pipeline.WatchdogAlert})
	}

	out := make([]PipelineStatus, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = r.pipeline(ctx, t)
		}()
	}
	wg.Wait()
	return out
}

func (r *StatusReporter) pipeline(ctx context.Context, t pipelineTarget) PipelineStatus {
	timeout := r.Pipeline.Timeout
	if timeout <= 0 {
		timeout = 4 * time.Second
	}
	p := PipelineStatus{Cluster: t.name, WatchdogAlert: t.watchdog}

	sctx, cancel := context.WithTimeout(ctx, timeout)
	st, err := t.am.Status(sctx)
	cancel()
	if err != nil {
		p.AMError = compactError(err)
		return p
	}
	p.AMVersion, p.AMCluster = st.Version, st.ClusterStatus

	actx, cancel := context.WithTimeout(ctx, timeout)
	ov, err := t.am.AlertsOverview(actx, t.watchdog)
	cancel()
	if err != nil {
		p.AlertsError = compactError(err)
		return p
	}
	p.Firing, p.Silenced = ov.Firing, ov.Silenced
	p.WatchdogSeen = ov.WatchdogSeen
	if ov.WatchdogSeen {
		p.WatchdogAge = time.Since(ov.WatchdogUpdatedAt).Truncate(time.Second)
	}
	return p
}

// FormatStatusTelegram renders the snapshot as Telegram HTML.
func FormatStatusTelegram(s StatusSnapshot) string {
	var b strings.Builder
	b.WriteString("🩺 <b>alertly status</b>\n")
	fmt.Fprintf(&b, "Version: %s (%s)\n", html.EscapeString(s.Version), html.EscapeString(s.Commit))
	fmt.Fprintf(&b, "Uptime: %s\n", s.Uptime)
	if s.Ready {
		b.WriteString("Ready: ✅\n")
	} else {
		fmt.Fprintf(&b, "Ready: ❌ %s\n", html.EscapeString(s.Reason))
	}
	for _, st := range s.Sinks {
		if st.Ready {
			fmt.Fprintf(&b, "  %s: ✅\n", sinkTitle(st.Name))
		} else {
			fmt.Fprintf(&b, "  %s: ❌ %s\n", sinkTitle(st.Name), html.EscapeString(st.Reason))
		}
	}
	if !s.LastCheck.IsZero() {
		label := "Last Telegram check"
		if len(s.Sinks) > 0 {
			label = "Last messenger check"
		}
		fmt.Fprintf(&b, "%s: %s ago\n", label, time.Since(s.LastCheck).Truncate(time.Second))
	}

	for _, p := range s.Pipelines {
		if p.Cluster == "" {
			b.WriteString("\n<b>Pipeline</b>\n")
		} else {
			fmt.Fprintf(&b, "\n<b>Pipeline — %s</b>\n", html.EscapeString(p.Cluster))
		}
		for _, line := range pipelineLines(p) {
			b.WriteString(html.EscapeString(line))
			b.WriteByte('\n')
		}
	}
	if a := s.Activity; a != nil {
		for _, line := range activityLines(a) {
			b.WriteString(html.EscapeString(line))
			b.WriteByte('\n')
		}
	}
	if len(s.Pipelines) > 0 {
		b.WriteString("\n")
	}

	for _, sz := range s.Sizes {
		fmt.Fprintf(&b, "%s: %d\n", html.EscapeString(sz.Label), sz.N)
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatStatusSlack renders the snapshot as Slack mrkdwn.
func FormatStatusSlack(s StatusSnapshot) string {
	esc := func(v string) string { return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(v) }
	var b strings.Builder
	b.WriteString(":stethoscope: *alertly status*\n")
	fmt.Fprintf(&b, "Version: %s (%s)\n", esc(s.Version), esc(s.Commit))
	fmt.Fprintf(&b, "Uptime: %s\n", s.Uptime)
	if s.Ready {
		b.WriteString("Ready: ✅\n")
	} else {
		fmt.Fprintf(&b, "Ready: ❌ %s\n", esc(s.Reason))
	}
	for _, st := range s.Sinks {
		if st.Ready {
			fmt.Fprintf(&b, "  %s: ✅\n", sinkTitle(st.Name))
		} else {
			fmt.Fprintf(&b, "  %s: ❌ %s\n", sinkTitle(st.Name), esc(st.Reason))
		}
	}
	for _, p := range s.Pipelines {
		if p.Cluster == "" {
			b.WriteString("\n*Pipeline*\n")
		} else {
			fmt.Fprintf(&b, "\n*Pipeline — %s*\n", esc(p.Cluster))
		}
		for _, line := range pipelineLines(p) {
			b.WriteString(esc(line))
			b.WriteByte('\n')
		}
	}
	if a := s.Activity; a != nil {
		for _, line := range activityLines(a) {
			b.WriteString(esc(line))
			b.WriteByte('\n')
		}
	}
	if len(s.Sizes) > 0 {
		b.WriteByte('\n')
	}
	for _, sz := range s.Sizes {
		fmt.Fprintf(&b, "%s: %d\n", esc(sz.Label), sz.N)
	}
	return strings.TrimRight(b.String(), "\n")
}

func pipelineLines(p PipelineStatus) []string {
	if p.AMError != "" {
		return []string{
			"Alertmanager: ❌ unreachable — " + p.AMError,
			"Watchdog/alerts: skipped (AM unreachable)",
		}
	}
	cluster := p.AMCluster
	if cluster == "" {
		cluster = "n/a"
	}
	lines := []string{fmt.Sprintf("Alertmanager: ✅ v%s, cluster %s", p.AMVersion, cluster)}
	if p.AlertsError != "" {
		return append(lines, "Alerts: ⚠️ query failed — "+p.AlertsError)
	}
	if p.WatchdogAlert != "" {
		switch {
		case !p.WatchdogSeen:
			lines = append(lines, fmt.Sprintf("Watchdog: ❌ %q not in AM — check Prometheus / rule evaluation", p.WatchdogAlert))
		case p.WatchdogAge > watchdogStaleAfter:
			lines = append(lines, fmt.Sprintf("Watchdog: ⚠️ stale, updated %s ago — Prometheus may be down", p.WatchdogAge))
		default:
			lines = append(lines, fmt.Sprintf("Watchdog: ✅ updated %s ago", p.WatchdogAge))
		}
	}
	return append(lines, fmt.Sprintf("Alerts in AM: %d firing, %d silenced", p.Firing, p.Silenced))
}

func activityLines(a *ActivitySnapshot) []string {
	var lines []string
	if a.LastWebhook.IsZero() {
		lines = append(lines, "Last webhook: none since start")
	} else {
		lines = append(lines, fmt.Sprintf("Last webhook: %s ago (%s)", time.Since(a.LastWebhook).Truncate(time.Second), a.WebhookSource))
	}
	if a.LastDelivery.IsZero() {
		lines = append(lines, "Last delivery: none since start")
	} else {
		lines = append(lines, fmt.Sprintf("Last delivery: %s ago", time.Since(a.LastDelivery).Truncate(time.Second)))
	}
	return lines
}

func sinkTitle(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
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

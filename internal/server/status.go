package server

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// SizeStat exposes the current size of one in-memory cache for /status output.
type SizeStat struct {
	Label string
	Len   func() int
}

// StatusReporter renders the /status chat-ops reply: build info, uptime,
// readiness and in-memory cache sizes. All fields are wired once at startup;
// Text is safe for concurrent use as long as Len funcs are.
type StatusReporter struct {
	StartedAt time.Time
	Version   string
	Commit    string
	Readiness ReadinessTracker
	Sizes     []SizeStat
}

func (r *StatusReporter) Text() string {
	var b strings.Builder
	b.WriteString("🩺 <b>alertly status</b>\n")
	fmt.Fprintf(&b, "Version: %s (%s)\n", html.EscapeString(r.Version), html.EscapeString(r.Commit))
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

	for _, s := range r.Sizes {
		fmt.Fprintf(&b, "%s: %d\n", html.EscapeString(s.Label), s.Len())
	}
	return strings.TrimRight(b.String(), "\n")
}

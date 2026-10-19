package notification

import (
	"fmt"
	"strings"
	"time"
)

type Notification struct {
	Source string
	// Cluster is the named cluster the webhook came in for; empty for the
	// implicit default cluster (legacy single-cluster mode).
	Cluster     string
	Fingerprint string
	Status      string
	Severity    string
	Title       string
	Body        string
	Labels      map[string]string
	Annotations map[string]string
	Links       []Link
	Timestamp   time.Time
	// StartsAt is when the alert started firing (Alertmanager); it tells
	// incidents of the same fingerprint apart. Zero for other sources.
	StartsAt time.Time
}

type Link struct {
	Title string
	URL   string
}

// StatusLine is the "Firing · cluster X · critical" context shared by the
// builtin layouts of every sink.
func (n Notification) StatusLine() []string {
	var status string
	switch n.Status {
	case "firing":
		status = "Firing"
	case "resolved":
		status = "Resolved"
		if d := n.Duration(); d > 0 {
			status += " after " + humanize(d)
		}
	case "":
		status = "Event"
	default:
		status = strings.ToUpper(n.Status[:1]) + n.Status[1:]
	}
	parts := []string{status}
	if n.Cluster != "" {
		parts = append(parts, "cluster "+n.Cluster)
	}
	if n.Severity != "" {
		parts = append(parts, n.Severity)
	}
	return parts
}

// Duration is how long a resolved alert fired; 0 when unknown.
func (n Notification) Duration() time.Duration {
	if n.Status != "resolved" || n.StartsAt.IsZero() || n.Timestamp.Before(n.StartsAt) {
		return 0
	}
	return n.Timestamp.Sub(n.StartsAt)
}

// humanize prints a coarse duration: 45s, 12m, 3h5m, 2d4h.
func humanize(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h, m := int(d.Hours()), int(d.Minutes())%60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

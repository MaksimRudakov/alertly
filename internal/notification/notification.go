package notification

import (
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

package notification

import "time"

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

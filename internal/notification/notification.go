package notification

import "time"

type Notification struct {
	Source      string
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

type ChatTarget struct {
	ChatID   int64
	ThreadID *int
}

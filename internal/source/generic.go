package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

// generic accepts alertly's own JSON contract so any tool that can POST JSON
// (GitLab CI, Jira automation, ArgoCD notifications webhook, plain curl) gets
// the full pipeline — dedup, splitting, threads, rate limiting — without a
// per-tool parser. The payload is a single event object or an array of them:
//
//	{
//	  "title":       "Deploy failed",            // required
//	  "body":        "pipeline #123 on main",    // optional
//	  "severity":    "critical",                 // optional, default "info"
//	  "status":      "firing",                   // optional, default "event"; part of the dedup key
//	  "fingerprint": "deploy-123",               // optional; hashed from content when absent
//	  "labels":      {"project": "shop"},        // optional
//	  "annotations": {"runbook": "..."},         // optional
//	  "links":       [{"title": "Pipeline", "url": "https://..."}],
//	  "timestamp":   "2026-07-05T12:00:00Z"      // optional, RFC3339
//	}
type generic struct{}

func NewGeneric() Source { return generic{} }

func (generic) Name() string { return "generic" }

type genericEvent struct {
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Severity    string            `json:"severity"`
	Status      string            `json:"status"`
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Links       []genericLink     `json:"links"`
	Timestamp   string            `json:"timestamp"`
}

type genericLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// maxGenericEvents bounds one request; MaxBodyBytes already caps the payload,
// this cap just keeps a single webhook from fanning out into a message flood.
const maxGenericEvents = 100

func (generic) Parse(body []byte) ([]notification.Notification, error) {
	trimmed := strings.TrimLeftFunc(string(body), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})

	var events []genericEvent
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(body, &events); err != nil {
			return nil, fmt.Errorf("generic: parse array: %w", err)
		}
	} else {
		var ev genericEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			return nil, fmt.Errorf("generic: parse object: %w", err)
		}
		events = []genericEvent{ev}
	}

	if len(events) == 0 {
		return nil, errors.New("generic: empty payload")
	}
	if len(events) > maxGenericEvents {
		return nil, fmt.Errorf("generic: too many events in one request: %d > %d", len(events), maxGenericEvents)
	}

	out := make([]notification.Notification, 0, len(events))
	for i, ev := range events {
		n, err := ev.toNotification()
		if err != nil {
			return nil, fmt.Errorf("generic: event %d: %w", i, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func (ev genericEvent) toNotification() (notification.Notification, error) {
	title := strings.TrimSpace(ev.Title)
	if title == "" {
		return notification.Notification{}, errors.New("title is required")
	}

	severity := strings.TrimSpace(ev.Severity)
	if severity == "" {
		severity = "info"
	}
	status := strings.TrimSpace(ev.Status)
	if status == "" {
		status = "event"
	}

	fp := strings.TrimSpace(ev.Fingerprint)
	if fp == "" {
		fp = genericFingerprint(title, ev.Body, status, ev.Labels)
	}

	links := make([]notification.Link, 0, len(ev.Links))
	for _, l := range ev.Links {
		if strings.TrimSpace(l.URL) == "" {
			continue
		}
		lt := l.Title
		if lt == "" {
			lt = "Link"
		}
		links = append(links, notification.Link{Title: lt, URL: l.URL})
	}

	labels := ev.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	annotations := ev.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}

	return notification.Notification{
		Source:      "generic",
		Fingerprint: fp,
		Status:      status,
		Severity:    severity,
		Title:       title,
		Body:        strings.TrimSpace(ev.Body),
		Labels:      labels,
		Annotations: annotations,
		Links:       links,
		Timestamp:   parseTime(ev.Timestamp),
	}, nil
}

// genericFingerprint hashes the content so dedup absorbs sender retries even
// when the sender did not provide an explicit fingerprint. Labels are folded
// in sorted order for stability.
func genericFingerprint(title, body, status string, labels map[string]string) string {
	parts := []string{title, body, status}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return fingerprint(parts...)
}

package source

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

type alertmanager struct{}

func NewAlertmanager() Source { return alertmanager{} }

func (alertmanager) Name() string { return "alertmanager" }

type amPayload struct {
	Status   string    `json:"status"`
	Receiver string    `json:"receiver"`
	Alerts   []amAlert `json:"alerts"`
}

type amAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

func (alertmanager) Parse(body []byte) ([]notification.Notification, error) {
	var p amPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("alertmanager: parse json: %w", err)
	}
	if len(p.Alerts) == 0 {
		return nil, fmt.Errorf("alertmanager: no alerts in payload")
	}

	out := make([]notification.Notification, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		out = append(out, alertToNotification(a))
	}
	return out, nil
}

func alertToNotification(a amAlert) notification.Notification {
	severity := strings.ToLower(strings.TrimSpace(a.Labels["severity"]))
	if severity == "" {
		severity = "info"
	}

	title := firstNonEmpty(a.Annotations["summary"], a.Labels["alertname"])
	body := firstNonEmpty(a.Annotations["description"], a.Annotations["message"])

	links := make([]notification.Link, 0, 2)
	if a.GeneratorURL != "" {
		links = append(links, notification.Link{Title: "Generator", URL: a.GeneratorURL})
	}
	if v := a.Annotations["runbook_url"]; v != "" {
		links = append(links, notification.Link{Title: "Runbook", URL: v})
	}

	ts := a.StartsAt
	if a.Status == "resolved" && !a.EndsAt.IsZero() {
		ts = a.EndsAt
	}

	return notification.Notification{
		Source:      "alertmanager",
		Fingerprint: a.Fingerprint,
		Status:      a.Status,
		Severity:    severity,
		Title:       title,
		Body:        body,
		Labels:      copyMap(a.Labels),
		Annotations: copyMap(a.Annotations),
		Links:       links,
		Timestamp:   ts,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

type kubewatch struct{}

func NewKubewatch() Source { return kubewatch{} }

func (kubewatch) Name() string { return "kubewatch" }

type kwEvent struct {
	EventMeta kwEventMeta `json:"eventmeta"`
	Text      string      `json:"text"`
	Time      string      `json:"time"`
}

type kwEventMeta struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Reason    string `json:"reason"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}

type kwLegacyEvent struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Reason    string    `json:"reason"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Time      time.Time `json:"time"`
}

func (kubewatch) Parse(body []byte) ([]notification.Notification, error) {
	var ev kwEvent
	if err := json.Unmarshal(body, &ev); err == nil && ev.EventMeta.Kind != "" {
		return []notification.Notification{kwToNotification(ev.EventMeta, ev.Text, parseTime(ev.Time))}, nil
	}

	var legacy kwLegacyEvent
	if err := json.Unmarshal(body, &legacy); err == nil && legacy.Kind != "" {
		return []notification.Notification{kwToNotification(kwEventMeta{
			Kind:      legacy.Kind,
			Name:      legacy.Name,
			Namespace: legacy.Namespace,
			Reason:    legacy.Reason,
			Type:      legacy.Type,
			Message:   legacy.Message,
		}, legacy.Message, legacy.Time)}, nil
	}

	return nil, fmt.Errorf("kubewatch: unsupported payload")
}

func kwToNotification(meta kwEventMeta, text string, ts time.Time) notification.Notification {
	severity := "info"
	if strings.EqualFold(meta.Type, "Warning") || strings.EqualFold(meta.Reason, "Failed") {
		severity = "warning"
	}

	body := strings.TrimSpace(text)
	if body == "" {
		body = strings.TrimSpace(meta.Message)
	}

	title := buildKWTitle(meta)
	fp := fingerprint(meta.Kind, meta.Namespace, meta.Name, meta.Reason, meta.Type)

	return notification.Notification{
		Source:      "kubewatch",
		Fingerprint: fp,
		Status:      "info",
		Severity:    severity,
		Title:       title,
		Body:        body,
		Labels: map[string]string{
			"kind":      meta.Kind,
			"namespace": meta.Namespace,
			"name":      meta.Name,
			"reason":    meta.Reason,
			"type":      meta.Type,
		},
		Annotations: map[string]string{},
		Timestamp:   ts,
	}
}

func buildKWTitle(meta kwEventMeta) string {
	var b strings.Builder
	if meta.Kind != "" {
		b.WriteString(meta.Kind)
		b.WriteByte(' ')
	}
	if meta.Namespace != "" {
		b.WriteString(meta.Namespace)
		b.WriteByte('/')
	}
	if meta.Name != "" {
		b.WriteString(meta.Name)
	}
	if meta.Reason != "" {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(meta.Reason)
	}
	if b.Len() == 0 {
		return "Kubewatch event"
	}
	return b.String()
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

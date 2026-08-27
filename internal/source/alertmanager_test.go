package source

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return data
}

func TestAlertmanagerFiring(t *testing.T) {
	notes, err := NewAlertmanager().Parse(loadFixture(t, "alertmanager_firing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notifications", len(notes))
	}
	n := notes[0]
	if n.Source != "alertmanager" {
		t.Errorf("source: %s", n.Source)
	}
	if n.Status != "firing" {
		t.Errorf("status: %s", n.Status)
	}
	if n.Severity != "critical" {
		t.Errorf("severity: %s", n.Severity)
	}
	if n.Title != "High memory usage on node-01" {
		t.Errorf("title: %s", n.Title)
	}
	if n.Body == "" {
		t.Error("body empty")
	}
	if n.Fingerprint != "abcdef1234567890" {
		t.Errorf("fingerprint: %s", n.Fingerprint)
	}
	if len(n.Links) != 1 {
		t.Errorf("links: %d", len(n.Links))
	}
	if n.Links[0].Title != "Runbook" {
		t.Errorf("link title: %s", n.Links[0].Title)
	}
}

func TestAlertmanagerResolved(t *testing.T) {
	notes, err := NewAlertmanager().Parse(loadFixture(t, "alertmanager_resolved.json"))
	if err != nil {
		t.Fatal(err)
	}
	if notes[0].Status != "resolved" {
		t.Errorf("status: %s", notes[0].Status)
	}
	if notes[0].Timestamp.Format("15:04") != "10:30" {
		t.Errorf("expected EndsAt timestamp, got %v", notes[0].Timestamp)
	}
}

func TestAlertmanagerGrouped(t *testing.T) {
	notes, err := NewAlertmanager().Parse(loadFixture(t, "alertmanager_grouped.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 {
		t.Fatalf("got %d notifications", len(notes))
	}
	if notes[2].Severity != "info" {
		t.Errorf("default severity: %s", notes[2].Severity)
	}
	if notes[2].Title != "PodCrashLoop" {
		t.Errorf("title fallback: %s", notes[2].Title)
	}
}

func TestAlertmanagerInvalid(t *testing.T) {
	if _, err := NewAlertmanager().Parse([]byte("not json")); err == nil {
		t.Error("expected error")
	}
	if _, err := NewAlertmanager().Parse([]byte(`{"alerts":[]}`)); err == nil {
		t.Error("expected error for empty alerts")
	}
}

func TestParseRejectsTooManyAlerts(t *testing.T) {
	alerts := make([]map[string]any, maxAlertsPerPayload+1)
	for i := range alerts {
		alerts[i] = map[string]any{
			"status":      "firing",
			"labels":      map[string]string{"alertname": fmt.Sprintf("A%d", i)},
			"fingerprint": fmt.Sprintf("fp%d", i),
		}
	}
	body, err := json.Marshal(map[string]any{"status": "firing", "alerts": alerts})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAlertmanager().Parse(body); err == nil {
		t.Fatal("want error for oversized payload")
	} else if !strings.Contains(err.Error(), "too many alerts") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseAcceptsMaxAlerts(t *testing.T) {
	alerts := make([]map[string]any, maxAlertsPerPayload)
	for i := range alerts {
		alerts[i] = map[string]any{
			"status":      "firing",
			"labels":      map[string]string{"alertname": fmt.Sprintf("A%d", i)},
			"fingerprint": fmt.Sprintf("fp%d", i),
		}
	}
	body, err := json.Marshal(map[string]any{"status": "firing", "alerts": alerts})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := NewAlertmanager().Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != maxAlertsPerPayload {
		t.Fatalf("want %d notifications, got %d", maxAlertsPerPayload, len(notes))
	}
}

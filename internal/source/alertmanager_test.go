package source

import (
	"os"
	"path/filepath"
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
	if len(n.Links) != 2 {
		t.Errorf("links: %d", len(n.Links))
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

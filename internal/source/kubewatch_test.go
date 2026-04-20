package source

import "testing"

func TestKubewatchEvent(t *testing.T) {
	notes, err := NewKubewatch().Parse(loadFixture(t, "kubewatch_event.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("notifs: %d", len(notes))
	}
	n := notes[0]
	if n.Source != "kubewatch" {
		t.Errorf("source: %s", n.Source)
	}
	if n.Severity != "info" {
		t.Errorf("severity: %s", n.Severity)
	}
	if n.Title != "Pod production/api-7c8f8b8f7b-xyz: Created" {
		t.Errorf("title: %s", n.Title)
	}
	if n.Labels["kind"] != "Pod" {
		t.Errorf("labels: %#v", n.Labels)
	}
	if n.Fingerprint == "" {
		t.Error("fingerprint empty")
	}
}

func TestKubewatchWarning(t *testing.T) {
	notes, err := NewKubewatch().Parse(loadFixture(t, "kubewatch_warning.json"))
	if err != nil {
		t.Fatal(err)
	}
	if notes[0].Severity != "warning" {
		t.Errorf("severity: %s", notes[0].Severity)
	}
}

func TestKubewatchFingerprintStable(t *testing.T) {
	a, _ := NewKubewatch().Parse(loadFixture(t, "kubewatch_event.json"))
	b, _ := NewKubewatch().Parse(loadFixture(t, "kubewatch_event.json"))
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Error("fingerprint should be stable")
	}
	w, _ := NewKubewatch().Parse(loadFixture(t, "kubewatch_warning.json"))
	if a[0].Fingerprint == w[0].Fingerprint {
		t.Error("different events should have different fingerprints")
	}
}

func TestKubewatchInvalid(t *testing.T) {
	if _, err := NewKubewatch().Parse([]byte("not json")); err == nil {
		t.Error("expected error")
	}
	if _, err := NewKubewatch().Parse([]byte(`{}`)); err == nil {
		t.Error("expected error for empty payload")
	}
}

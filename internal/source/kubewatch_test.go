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

// Same object+reason but different message must not share a fingerprint —
// otherwise dedup collapses distinct events (e.g. CrashLoopBackOff with
// changing restart counts) for the whole TTL.
func TestKubewatchFingerprintIncludesMessage(t *testing.T) {
	payload := func(msg string) []byte {
		return []byte(`{"eventmeta":{"kind":"Pod","name":"api","namespace":"prod","reason":"BackOff","type":"Warning"},"text":"` + msg + `","time":"2026-01-01T00:00:00Z"}`)
	}
	a, err := NewKubewatch().Parse(payload("restart count 1"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewKubewatch().Parse(payload("restart count 2"))
	if a[0].Fingerprint == b[0].Fingerprint {
		t.Error("different messages should produce different fingerprints")
	}
	c, _ := NewKubewatch().Parse(payload("restart count 1"))
	if a[0].Fingerprint != c[0].Fingerprint {
		t.Error("identical payloads (caller retry) should share a fingerprint")
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

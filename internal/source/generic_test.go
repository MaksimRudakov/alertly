package source

import (
	"strings"
	"testing"
)

func TestGenericSingleObject(t *testing.T) {
	body := []byte(`{
		"title": "Deploy failed",
		"body": "pipeline #123 on main",
		"severity": "critical",
		"status": "firing",
		"labels": {"project": "shop"},
		"links": [{"title": "Pipeline", "url": "https://gitlab/p/123"}],
		"timestamp": "2026-07-05T12:00:00Z"
	}`)
	notes, err := NewGeneric().Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes: %d", len(notes))
	}
	n := notes[0]
	if n.Source != "generic" || n.Title != "Deploy failed" || n.Severity != "critical" || n.Status != "firing" {
		t.Errorf("notification: %+v", n)
	}
	if n.Fingerprint == "" {
		t.Error("fingerprint must be computed when absent")
	}
	if len(n.Links) != 1 || n.Links[0].URL != "https://gitlab/p/123" {
		t.Errorf("links: %+v", n.Links)
	}
	if n.Timestamp.IsZero() {
		t.Error("timestamp should parse")
	}
}

func TestGenericArray(t *testing.T) {
	body := []byte(`[{"title": "a"}, {"title": "b"}]`)
	notes, err := NewGeneric().Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("notes: %d", len(notes))
	}
}

func TestGenericDefaults(t *testing.T) {
	notes, err := NewGeneric().Parse([]byte(`{"title": "x"}`))
	if err != nil {
		t.Fatal(err)
	}
	n := notes[0]
	if n.Severity != "info" || n.Status != "event" {
		t.Errorf("defaults: severity=%q status=%q", n.Severity, n.Status)
	}
	if n.Labels == nil || n.Annotations == nil {
		t.Error("maps must be non-nil for templates")
	}
}

func TestGenericExplicitFingerprint(t *testing.T) {
	notes, err := NewGeneric().Parse([]byte(`{"title": "x", "fingerprint": "my-fp"}`))
	if err != nil {
		t.Fatal(err)
	}
	if notes[0].Fingerprint != "my-fp" {
		t.Errorf("fingerprint: %q", notes[0].Fingerprint)
	}
}

func TestGenericFingerprintStable(t *testing.T) {
	payload := []byte(`{"title": "x", "body": "b", "labels": {"a": "1", "z": "2"}}`)
	a, _ := NewGeneric().Parse(payload)
	b, _ := NewGeneric().Parse(payload)
	if a[0].Fingerprint != b[0].Fingerprint {
		t.Error("fingerprint should be stable across identical payloads")
	}
	c, _ := NewGeneric().Parse([]byte(`{"title": "x", "body": "other"}`))
	if a[0].Fingerprint == c[0].Fingerprint {
		t.Error("different content should differ")
	}
}

func TestGenericValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing title", `{"body": "no title"}`},
		{"empty array", `[]`},
		{"not json", `plain text`},
		{"array item without title", `[{"title": "ok"}, {"body": "bad"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewGeneric().Parse([]byte(c.body)); err == nil {
				t.Errorf("expected error for %s", c.name)
			}
		})
	}
}

func TestGenericTooManyEvents(t *testing.T) {
	items := make([]string, 101)
	for i := range items {
		items[i] = `{"title": "x"}`
	}
	body := "[" + strings.Join(items, ",") + "]"
	if _, err := NewGeneric().Parse([]byte(body)); err == nil {
		t.Error("expected error for >100 events")
	}
}

func TestGenericLinkWithoutURLSkipped(t *testing.T) {
	notes, err := NewGeneric().Parse([]byte(`{"title": "x", "links": [{"title": "empty"}, {"url": "https://a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes[0].Links) != 1 || notes[0].Links[0].Title != "Link" {
		t.Errorf("links: %+v", notes[0].Links)
	}
}

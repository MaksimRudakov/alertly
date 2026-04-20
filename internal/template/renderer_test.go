package template

import (
	"strings"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

func TestRenderDefault(t *testing.T) {
	r, err := New(map[string]string{
		DefaultName: `{{ severity_emoji .Severity }} <b>{{ escape_html .Title }}</b>` + "\n" +
			`{{ if .Body }}{{ escape_html .Body }}{{ end }}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render(DefaultName, notification.Notification{
		Severity: "critical",
		Title:    "Pod <down>",
		Body:     "ns & app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "🔥") {
		t.Errorf("missing severity emoji: %q", out)
	}
	if !strings.Contains(out, "Pod &lt;down&gt;") {
		t.Errorf("missing escaped title: %q", out)
	}
	if !strings.Contains(out, "ns &amp; app") {
		t.Errorf("missing escaped body: %q", out)
	}
}

func TestRenderUnknownFallsBackToDefault(t *testing.T) {
	r, err := New(map[string]string{DefaultName: "DEF {{ .Title }}"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("does-not-exist", notification.Notification{Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "DEF X" {
		t.Errorf("fallback failed: %q", out)
	}
}

func TestRenderMissingDefault(t *testing.T) {
	if _, err := New(map[string]string{"only": "X"}); err == nil {
		t.Fatal("expected error when default is missing")
	}
}

func TestRenderInvalidTemplate(t *testing.T) {
	if _, err := New(map[string]string{DefaultName: "{{ .Title "}); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSeverityEmoji(t *testing.T) {
	cases := map[string]string{
		"critical": "🔥",
		"warning":  "⚠️",
		"info":     "ℹ️",
		"":         "ℹ️",
		"unknown":  "ℹ️",
	}
	for in, want := range cases {
		if got := SeverityEmoji(in); got != want {
			t.Errorf("SeverityEmoji(%q) = %q want %q", in, got, want)
		}
	}
}

func TestEscapeHTML(t *testing.T) {
	if got := EscapeHTML("a & <b> c"); got != "a &amp; &lt;b&gt; c" {
		t.Errorf("escape: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("hello", 10); got != "hello" {
		t.Errorf("no-trunc: %q", got)
	}
	if got := Truncate("hello world", 5); got != "hello…" {
		t.Errorf("trunc: %q", got)
	}
	if got := Truncate("привет мир", 6); got != "привет…" {
		t.Errorf("unicode: %q", got)
	}
	if got := Truncate("x", 0); got != "" {
		t.Errorf("zero: %q", got)
	}
}

func TestJoin(t *testing.T) {
	if got := JoinStrings([]string{"a", "b", "c"}, ","); got != "a,b,c" {
		t.Errorf("join: %q", got)
	}
}

func TestHumanizeDuration(t *testing.T) {
	if got := HumanizeDuration(time.Time{}); got != "unknown" {
		t.Errorf("zero time: %q", got)
	}
	got := HumanizeDuration(time.Now().Add(-2 * time.Minute))
	if !strings.HasSuffix(got, "ago") {
		t.Errorf("ago expected: %q", got)
	}
}

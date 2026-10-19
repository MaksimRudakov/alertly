package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// Classify drives readiness: only server-side degradation counts.
func TestSinkClassify(t *testing.T) {
	s := &Sink{}
	cases := []struct {
		name string
		err  error
		want sink.ErrorClass
	}{
		{"canceled context", context.Canceled, sink.ErrCanceled},
		{"deadline exceeded", fmt.Errorf("send: %w", context.DeadlineExceeded), sink.ErrCanceled},
		{"network error", errors.New("dial tcp: connection refused"), sink.ErrServer},
		{"api 400", &APIError{StatusCode: 400}, sink.ErrClient},
		{"api 429", &APIError{StatusCode: 429}, sink.ErrRateLimited},
		{"api 500", &APIError{StatusCode: 500}, sink.ErrServer},
		{"api 503", &APIError{StatusCode: 503}, sink.ErrServer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.Classify(c.err); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestParseTarget(t *testing.T) {
	chat, thread, err := ParseTarget(sink.Target{Sink: sink.Telegram, Chat: "-100", Thread: "42"})
	if err != nil || chat != -100 || thread == nil || *thread != 42 {
		t.Fatalf("got %d %v %v", chat, thread, err)
	}
	if _, thread, _ := ParseTarget(sink.Target{Sink: sink.Telegram, Chat: "-100"}); thread != nil {
		t.Error("no thread expected")
	}
	if _, _, err := ParseTarget(sink.Target{Sink: sink.Telegram, Chat: "abc"}); err == nil {
		t.Error("invalid chat must fail")
	}
}

func TestSinkRender_TemplateAndBuiltin(t *testing.T) {
	r, err := tmpl.New(map[string]string{tmpl.DefaultName: "T {{ .Title }}"})
	if err != nil {
		t.Fatal(err)
	}
	n := notification.Notification{
		Title: "Disk <full>", Status: "firing", Severity: "critical", Cluster: "k8s-prod",
		Body:   "used 95%",
		Labels: map[string]string{"alertname": "DiskFull", "namespace": "db"},
		Links:  []notification.Link{{Title: "Runbook", URL: `https://wiki/x?q="a"`}},
	}

	parts, err := NewSink(nil, r, false, nil).Render("alertmanager", n)
	if err != nil || len(parts) != 1 || parts[0].Text != "T Disk <full>" {
		t.Fatalf("template render: %+v %v", parts, err)
	}

	parts, err = NewSink(nil, r, true, []string{"alertname", "namespace", "missing"}).Render("alertmanager", n)
	if err != nil || len(parts) != 1 {
		t.Fatalf("builtin render: %+v %v", parts, err)
	}
	got := parts[0].Text
	for _, want := range []string{
		"🔥 <b>Disk &lt;full&gt;</b>",
		"Firing · cluster k8s-prod · critical",
		"used 95%",
		"alertname: <code>DiskFull</code>",
		"namespace: <code>db</code>",
		`<a href="https://wiki/x?q=&quot;a&quot;">Runbook</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("builtin output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "missing") {
		t.Errorf("absent label must be skipped:\n%s", got)
	}
}

func TestSinkRender_TemplateErrorIsRenderError(t *testing.T) {
	r, err := tmpl.New(map[string]string{tmpl.DefaultName: `{{ index .Labels 1 }}`})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSink(nil, r, false, nil).Render("x", notification.Notification{Labels: map[string]string{}})
	var re *sink.RenderError
	if !errors.As(err, &re) {
		t.Fatalf("want *sink.RenderError, got %T %v", err, err)
	}
}

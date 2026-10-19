package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestSink_ReplyAndEdit(t *testing.T) {
	var mu sync.Mutex
	var bodies = map[string]map[string]any{}
	notModified := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		bodies[method] = b
		mu.Unlock()
		if method == "editMessageText" && notModified {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
	}))
	defer srv.Close()
	s := NewSink(New(Config{APIURL: srv.URL, Token: "t", MaxAttempts: 1}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), nil, true, nil)
	ctx := context.Background()

	if _, err := s.Send(ctx, sink.Target{Sink: sink.Telegram, Chat: "-100", Thread: "3", ReplyTo: "42"}, sink.Part{Text: "resolved"}, nil); err != nil {
		t.Fatal(err)
	}
	rp, _ := bodies["sendMessage"]["reply_parameters"].(map[string]any)
	if rp["message_id"] != float64(42) || rp["allow_sending_without_reply"] != true || bodies["sendMessage"]["message_thread_id"] != float64(3) {
		t.Errorf("reply must target message 42 in topic 3 and survive its deletion: %v", bodies["sendMessage"])
	}
	if _, err := s.Send(ctx, sink.Target{Sink: sink.Telegram, Chat: "-100"}, sink.Part{Text: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies["sendMessage"]["reply_parameters"]; ok {
		t.Error("plain send must not carry reply_parameters")
	}

	ref := sink.MessageRef{Sink: sink.Telegram, Chat: "-100", ID: "42"}
	if err := s.Edit(ctx, ref, sink.Part{Text: "✅ resolved"}, nil); err != nil {
		t.Fatal(err)
	}
	if e := bodies["editMessageText"]; e["message_id"] != float64(42) || e["text"] != "✅ resolved" || e["reply_markup"] != nil {
		t.Errorf("edit must rewrite text and drop the keyboard: %v", e)
	}
	notModified = true
	if err := s.Edit(ctx, ref, sink.Part{Text: "✅ resolved"}, nil); err != nil {
		t.Errorf("an unchanged message is not an error: %v", err)
	}
}

func TestRenderBuiltin_ResolvedIsCheckMark(t *testing.T) {
	got := RenderBuiltin(notification.Notification{Title: "Disk", Status: "resolved", Severity: "critical"}, nil)
	if !strings.HasPrefix(got, "✅ <b>Disk</b>") {
		t.Errorf("resolved must lead with ✅, got %q", got)
	}
	if got := RenderBuiltin(notification.Notification{Title: "Disk", Status: "firing", Severity: "critical"}, nil); !strings.HasPrefix(got, "🔥") {
		t.Errorf("firing keeps the severity emoji, got %q", got)
	}
}

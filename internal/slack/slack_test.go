package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

func init() { metrics.Init() }

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeSlack struct {
	srv   *httptest.Server
	calls atomic.Int32
	mu    sync.Mutex
	last  map[string]any
	auth  string
	// respond returns status, body for the n-th call (1-based).
	respond func(n int32, method string) (int, string, http.Header)
}

func newFakeSlack(t *testing.T, respond func(n int32, method string) (int, string, http.Header)) *fakeSlack {
	f := &fakeSlack{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.last, f.auth = body, r.Header.Get("Authorization")
		f.mu.Unlock()
		status, resp, hdr := 200, `{"ok":true,"ts":"1712345678.000100"}`, http.Header(nil)
		if f.respond != nil {
			status, resp, hdr = f.respond(n, strings.TrimPrefix(r.URL.Path, "/"))
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) client(attempts int) Client {
	return New(Config{APIURL: f.srv.URL, Token: "xoxb-secret", MaxAttempts: attempts,
		InitialBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond}, NewLimiter(1000, 1000), discard)
}

func TestPostMessage_PayloadAndAuth(t *testing.T) {
	f := newFakeSlack(t, nil)
	ts, err := f.client(1).PostMessage(context.Background(), Message{Channel: "C0123ABCD", Text: "hi", ThreadTS: "1.2"})
	if err != nil || ts != "1712345678.000100" {
		t.Fatalf("ts=%q err=%v", ts, err)
	}
	if f.auth != "Bearer xoxb-secret" {
		t.Errorf("auth header: %q", f.auth)
	}
	if f.last["channel"] != "C0123ABCD" || f.last["thread_ts"] != "1.2" || f.last["unfurl_links"] != false {
		t.Errorf("payload: %v", f.last)
	}
}

func TestCall_RetriesAndClassification(t *testing.T) {
	cases := []struct {
		name      string
		responses []string // "status|body"
		wantCalls int32
		wantErr   bool
		wantClass sink.ErrorClass
	}{
		{"client error not retried", []string{`200|{"ok":false,"error":"channel_not_found"}`}, 1, true, sink.ErrClient},
		{"429 retried", []string{`429|{}`, `200|{"ok":true,"ts":"1"}`}, 2, false, 0},
		{"5xx retried", []string{`503|oops`, `200|{"ok":true,"ts":"1"}`}, 2, false, 0},
		{"internal_error retried", []string{`200|{"ok":false,"error":"internal_error"}`, `200|{"ok":true,"ts":"1"}`}, 2, false, 0},
		{"5xx exhausted is server error", []string{`500|x`, `500|x`, `500|x`}, 3, true, sink.ErrServer},
		{"ratelimited code", []string{`200|{"ok":false,"error":"ratelimited"}`, `200|{"ok":false,"error":"ratelimited"}`, `200|{"ok":false,"error":"ratelimited"}`}, 3, true, sink.ErrRateLimited},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeSlack(t, func(n int32, _ string) (int, string, http.Header) {
				r := c.responses[min(int(n), len(c.responses))-1]
				status, body, _ := strings.Cut(r, "|")
				code := 200
				switch status {
				case "429":
					code = 429
				case "503":
					code = 503
				case "500":
					code = 500
				}
				return code, body, http.Header{"Retry-After": {"0"}}
			})
			_, err := f.client(3).PostMessage(context.Background(), Message{Channel: "C0123ABCD", Text: "x"})
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if got := f.calls.Load(); got != c.wantCalls {
				t.Errorf("calls=%d want %d", got, c.wantCalls)
			}
			if err != nil {
				if got := (&Sink{}).Classify(err); got != c.wantClass {
					t.Errorf("class=%v want %v", got, c.wantClass)
				}
				if strings.Contains(err.Error(), "xoxb-secret") {
					t.Error("token leaked into error")
				}
			}
		})
	}
}

func TestCall_DeadlineSkip(t *testing.T) {
	f := newFakeSlack(t, func(int32, string) (int, string, http.Header) {
		return 429, `{}`, http.Header{"Retry-After": {"5"}}
	})
	c := New(Config{APIURL: f.srv.URL, Token: "t", MaxAttempts: 5, InitialBackoff: time.Second, MaxBackoff: 10 * time.Second}, nil, discard)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.PostMessage(ctx, Message{Channel: "C0123ABCD"})
	if err == nil || time.Since(start) > time.Second || f.calls.Load() != 1 {
		t.Fatalf("expected immediate abort without sleeping past the deadline: err=%v calls=%d took=%v", err, f.calls.Load(), time.Since(start))
	}
}

func TestAuthTest(t *testing.T) {
	f := newFakeSlack(t, func(_ int32, method string) (int, string, http.Header) {
		if method != "auth.test" {
			return 404, "", nil
		}
		return 200, `{"ok":false,"error":"invalid_auth"}`, nil
	})
	err := f.client(1).AuthTest(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "invalid_auth" {
		t.Fatalf("want invalid_auth, got %v", err)
	}
}

func decodeParts(t *testing.T, parts []sink.Part) []partPayload {
	t.Helper()
	out := make([]partPayload, len(parts))
	for i, p := range parts {
		if err := json.Unmarshal(p.Payload, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func blockTypes(pp partPayload) []string {
	var types []string
	for _, b := range pp.Attachments[0].Blocks {
		var v struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(b, &v)
		types = append(types, v.Type)
	}
	return types
}

func TestRenderBuiltin(t *testing.T) {
	n := notification.Notification{
		Title: "Disk <full> & more", Status: "firing", Severity: "critical", Cluster: "k8s-prod",
		Body:   "used *95%* <@U123>",
		Labels: map[string]string{"alertname": "DiskFull", "namespace": "db"},
		Links:  []notification.Link{{Title: "Runbook", URL: "https://wiki/x?a=1|2"}},
	}
	parts, err := NewSink(nil, nil, true, []string{"alertname", "namespace", "absent"}).Render("alertmanager", n)
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts=%d err=%v", len(parts), err)
	}
	pp := decodeParts(t, parts)[0]
	if pp.Attachments[0].Color != colorCritical {
		t.Errorf("color: %s", pp.Attachments[0].Color)
	}
	if got := strings.Join(blockTypes(pp), ","); got != "header,context,section,section,context" {
		t.Errorf("blocks: %s", got)
	}
	raw := string(parts[0].Payload)
	for _, want := range []string{
		`Disk \u003cfull\u003e \u0026 more`,          // header is plain_text: no escaping needed
		`Firing · cluster k8s-prod · critical`,       // status context
		`used *95%* \u0026lt;@U123\u0026gt;`,         // mention neutralised
		`*alertname*\n` + "`DiskFull`",               // key field
		`\u003chttps://wiki/x?a=1%7C2|Runbook\u003e`, // link: pipe cannot break <url|text>
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("payload missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "absent") {
		t.Error("absent label must be skipped")
	}
	if !strings.Contains(parts[0].Text, "Disk <full> & more") {
		t.Errorf("fallback text: %q", parts[0].Text)
	}

	n.Status = "resolved"
	parts, _ = NewSink(nil, nil, true, nil).Render("x", n)
	if decodeParts(t, parts)[0].Attachments[0].Color != colorResolved {
		t.Error("resolved must be green")
	}
}

func TestRenderBuiltin_LongBodySplits(t *testing.T) {
	body := strings.Repeat(strings.Repeat("word ", 500)+"\n\n", 60) // ~150k chars
	parts, err := NewSink(nil, nil, true, []string{"alertname"}).Render("x", notification.Notification{
		Title: "big", Body: body, Labels: map[string]string{"alertname": "A"},
	})
	if err != nil || len(parts) < 2 {
		t.Fatalf("expected continuation messages, got %d (%v)", len(parts), err)
	}
	pps := decodeParts(t, parts)
	for i, pp := range pps {
		types := blockTypes(pp)
		if len(types) > 50 {
			t.Errorf("part %d has %d blocks", i, len(types))
		}
		for _, b := range pp.Attachments[0].Blocks {
			var s struct {
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			}
			_ = json.Unmarshal(b, &s)
			if utf8.RuneCountInString(s.Text.Text) > sectionTextLimit {
				t.Errorf("part %d: section over %d chars", i, sectionTextLimit)
			}
		}
	}
	if blockTypes(pps[0])[0] != "header" {
		t.Error("header must lead the first message")
	}
	last := blockTypes(pps[len(pps)-1])
	if last[len(last)-1] != "section" || !strings.Contains(string(parts[len(parts)-1].Payload), "alertname") {
		t.Error("label fields must close the last message")
	}
}

func TestRenderTemplate(t *testing.T) {
	r, err := tmpl.New(map[string]string{
		tmpl.DefaultName:     "tg {{ .Title }}",
		"slack.default":      "*{{ escape_slack .Title }}*",
		"slack.alertmanager": "AM {{ escape_slack .Title }}",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewSink(nil, r, false, nil)
	n := notification.Notification{Title: "a<b"}

	parts, err := s.Render("alertmanager", n)
	if err != nil || !strings.Contains(string(parts[0].Payload), `AM a\u0026lt;b`) {
		t.Fatalf("source template: %s %v", parts[0].Payload, err)
	}
	parts, err = s.Render("kubewatch", n)
	if err != nil || !strings.Contains(string(parts[0].Payload), `*a\u0026lt;b*`) {
		t.Fatalf("slack.default fallback: %s %v", parts[0].Payload, err)
	}
	if strings.Contains(string(parts[0].Payload), "tg ") {
		t.Error("Telegram default template must never be used for Slack")
	}
}

func TestSink_SendUsesPayload(t *testing.T) {
	f := newFakeSlack(t, nil)
	s := NewSink(f.client(1), nil, true, nil)
	parts, _ := s.Render("x", notification.Notification{Title: "T", Severity: "warning"})
	ref, err := s.Send(context.Background(), sink.Target{Sink: sink.Slack, Chat: "C0123ABCD", Thread: "9.9"}, parts[0], nil)
	if err != nil || ref.ID != "1712345678.000100" || ref.Chat != "C0123ABCD" {
		t.Fatalf("ref=%+v err=%v", ref, err)
	}
	att, _ := f.last["attachments"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["color"] != colorWarning || f.last["thread_ts"] != "9.9" {
		t.Errorf("sent payload: %v", f.last)
	}
}

func TestSplitText(t *testing.T) {
	chunks := splitText("aaa bbb\n\nccc ddd", 9)
	if len(chunks) != 2 || chunks[0] != "aaa bbb" || chunks[1] != "ccc ddd" {
		t.Errorf("paragraph split: %q", chunks)
	}
	emoji := strings.Repeat("🔥", 10)
	for _, c := range splitText(emoji, 3) {
		if !utf8.ValidString(c) || utf8.RuneCountInString(c) > 3 {
			t.Errorf("rune-safe split broke: %q", c)
		}
	}
}

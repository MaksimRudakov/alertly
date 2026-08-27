package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// deadlineProbeClient records the ctx budget each send was given and can stall
// so the request budget runs out mid-payload.
type deadlineProbeClient struct {
	mu        sync.Mutex
	budgets   []time.Duration // per send; -1 when the ctx carried no deadline
	sendDelay time.Duration
}

func (c *deadlineProbeClient) SendMessage(ctx context.Context, chatID int64, threadID *int, text string, opts *telegram.SendOptions) (int64, error) {
	budget := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}
	c.mu.Lock()
	c.budgets = append(c.budgets, budget)
	c.mu.Unlock()
	if c.sendDelay > 0 {
		select {
		case <-time.After(c.sendDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 1, nil
}

func (c *deadlineProbeClient) observed() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.budgets))
	copy(out, c.budgets)
	return out
}

func (c *deadlineProbeClient) GetMe(context.Context) error { return nil }
func (c *deadlineProbeClient) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}
func (c *deadlineProbeClient) AnswerCallbackQuery(context.Context, string, string, bool) error {
	return nil
}
func (c *deadlineProbeClient) EditMessageText(context.Context, int64, int64, string, *telegram.InlineKeyboardMarkup) error {
	return nil
}
func (c *deadlineProbeClient) EditMessageReplyMarkup(context.Context, int64, int64, *telegram.InlineKeyboardMarkup) error {
	return nil
}

func newProbeServer(t *testing.T, tg telegram.Client, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	metrics.Init()

	renderer, err := tmpl.New(map[string]string{tmpl.DefaultName: `{{ .Title }}`})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Server
	cfg.WriteTimeout = writeTimeout

	s := New(cfg, Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sources:   map[string]source.Source{"alertmanager": source.NewAlertmanager()},
		Renderer:  renderer,
		Telegram:  tg,
		Readiness: NewReadiness(),
		AuthToken: authToken,
		Registry:  prometheus.NewRegistry(),
		Activity:  NewActivityTracker(),
	})
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func alertsPayload(t *testing.T, n int) []byte {
	t.Helper()
	type alert struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Fingerprint string            `json:"fingerprint"`
	}
	alerts := make([]alert, 0, n)
	for i := 0; i < n; i++ {
		alerts = append(alerts, alert{
			Status:      "firing",
			Labels:      map[string]string{"alertname": fmt.Sprintf("Alert%d", i), "severity": "critical"},
			Annotations: map[string]string{"summary": fmt.Sprintf("summary %d", i)},
			Fingerprint: fmt.Sprintf("fp-%d", i),
		})
	}
	body, err := json.Marshal(map[string]any{"status": "firing", "alerts": alerts})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRequestTimeoutMiddlewareSetsDeadline(t *testing.T) {
	var (
		got time.Duration
		ok  bool
	)
	h := requestTimeoutMiddleware(30 * time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var dl time.Time
		dl, ok = r.Context().Deadline()
		got = time.Until(dl)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/alertmanager/1", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("handler context has no deadline")
	}
	// 30s write timeout minus the 1s response-writing margin.
	if got < 28*time.Second || got > 29*time.Second+100*time.Millisecond {
		t.Fatalf("unexpected budget %s, want ~29s", got)
	}
}

func TestRequestTimeoutMiddlewareNoWriteTimeout(t *testing.T) {
	var ok bool
	h := requestTimeoutMiddleware(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/alertmanager/1", nil))
	if ok {
		t.Fatal("want no deadline when write timeout is disabled")
	}
}

// The Telegram client aborts a retry when the remaining ctx budget is too
// short; that only works if the webhook request carries a deadline at all.
func TestWebhookSendCarriesDeadline(t *testing.T) {
	tg := &deadlineProbeClient{}
	ts := newProbeServer(t, tg, 30*time.Second)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100123", alertsPayload(t, 1), nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	budgets := tg.observed()
	if len(budgets) != 1 {
		t.Fatalf("want 1 send, got %d", len(budgets))
	}
	if budgets[0] <= 0 {
		t.Fatalf("send ran with no deadline (budget %s)", budgets[0])
	}
	if budgets[0] > 29*time.Second+100*time.Millisecond {
		t.Fatalf("budget %s exceeds the write timeout margin", budgets[0])
	}
}

func TestWebhookStopsAtDeadlineAndReportsPartial(t *testing.T) {
	tg := &deadlineProbeClient{sendDelay: 120 * time.Millisecond}
	// 1.2s write timeout − 1s margin = 200ms of send budget: two sends fit,
	// the remaining alerts must not be attempted.
	ts := newProbeServer(t, tg, 1200*time.Millisecond)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100123", alertsPayload(t, 6), nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207 when the budget runs out", resp.StatusCode)
	}
	if n := len(tg.observed()); n == 0 || n >= 6 {
		t.Fatalf("want a partial run, got %d of 6 sends", n)
	}
}

func TestWebhookNoDeadlineWhenWriteTimeoutDisabled(t *testing.T) {
	tg := &deadlineProbeClient{}
	ts := newProbeServer(t, tg, 0)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100123", alertsPayload(t, 1), nil)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if budgets := tg.observed(); len(budgets) != 1 || budgets[0] != -1 {
		t.Fatalf("want a deadline-less send, got %v", budgets)
	}
}

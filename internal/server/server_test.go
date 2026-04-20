package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

const authToken = "secret-token"

type capturedSend struct {
	ChatID    int64
	ThreadID  *int
	Text      string
	ParseMode string
}

type telegramRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	sent []capturedSend
}

func newTelegramRecorder(t *testing.T) *telegramRecorder {
	t.Helper()
	r := &telegramRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getMe"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"bot"}}`)
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			var p struct {
				ChatID                int64  `json:"chat_id"`
				Text                  string `json:"text"`
				ParseMode             string `json:"parse_mode"`
				DisableWebPagePreview bool   `json:"disable_web_page_preview"`
				MessageThreadID       *int   `json:"message_thread_id,omitempty"`
			}
			_ = json.NewDecoder(req.Body).Decode(&p)
			r.mu.Lock()
			r.sent = append(r.sent, capturedSend{
				ChatID:    p.ChatID,
				ThreadID:  p.MessageThreadID,
				Text:      p.Text,
				ParseMode: p.ParseMode,
			})
			r.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *telegramRecorder) Sent() []capturedSend {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedSend, len(r.sent))
	copy(out, r.sent)
	return out
}

func (r *telegramRecorder) Reset() {
	r.mu.Lock()
	r.sent = nil
	r.mu.Unlock()
}

func newTestServer(t *testing.T, rec *telegramRecorder, perChatRPS float64) *httptest.Server {
	t.Helper()

	registry := prometheus.NewRegistry()
	metrics.Init()

	limiter := telegram.NewLimiter(1000, perChatRPS)
	tg := telegram.New(telegram.Config{
		APIURL:         rec.srv.URL,
		Token:          "tok",
		ParseMode:      "HTML",
		RequestTimeout: 5 * time.Second,
		MaxAttempts:    1,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	}, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)))

	renderer, err := tmpl.New(map[string]string{
		tmpl.DefaultName: `{{ .Title }}{{ if .Body }}: {{ .Body }}{{ end }}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sources:   map[string]source.Source{"alertmanager": source.NewAlertmanager(), "kubewatch": source.NewKubewatch()},
		Renderer:  renderer,
		Telegram:  tg,
		Readiness: NewReadiness(),
		AuthToken: authToken,
		Registry:  registry,
	}
	srvCfg := config.Default().Server
	s := New(srvCfg, deps)
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func doPost(t *testing.T, base, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestE2EAlertmanagerSingleChat(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-1001234567890", loadFixture(t, "alertmanager_firing.json"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	sent := rec.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sent))
	}
	if sent[0].ChatID != -1001234567890 {
		t.Errorf("chat id: %d", sent[0].ChatID)
	}
	if sent[0].ParseMode != "HTML" {
		t.Errorf("parse mode: %s", sent[0].ParseMode)
	}
	if !strings.Contains(sent[0].Text, "High memory") {
		t.Errorf("text: %s", sent[0].Text)
	}
}

func TestE2EAuthRequired(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/alertmanager/-100", bytes.NewReader([]byte("{}")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/alertmanager/-100", bytes.NewReader([]byte("{}")))
	req2.Header.Set("Authorization", "Bearer wrong")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Errorf("expected 401 for wrong token, got %d", resp2.StatusCode)
	}
}

func TestE2EMultiChat(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100123,-100456", loadFixture(t, "alertmanager_firing.json"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if len(rec.Sent()) != 2 {
		t.Errorf("expected 2 sends, got %d", len(rec.Sent()))
	}
}

func TestE2EThreadID(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100123:42", loadFixture(t, "alertmanager_firing.json"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	sent := rec.Sent()
	if len(sent) != 1 || sent[0].ThreadID == nil || *sent[0].ThreadID != 42 {
		t.Errorf("thread mismatch: %+v", sent)
	}
}

func TestE2ELongMessageSplit(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	huge := strings.Repeat("X", 5000)
	payload := []byte(`{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"severity":"info","alertname":"Big"},"annotations":{"summary":"Big","description":"` + huge + `"},"startsAt":"2026-04-21T10:00:00Z","endsAt":"0001-01-01T00:00:00Z","generatorURL":"http://x","fingerprint":"fp"}]}`)

	resp := doPost(t, ts.URL, "/v1/alertmanager/-100", payload, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	sent := rec.Sent()
	if len(sent) != 2 {
		t.Fatalf("expected 2 split parts, got %d", len(sent))
	}
}

func TestE2EPerChatRateLimit(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1)

	chat := "-1009999999999"
	payload := loadFixture(t, "alertmanager_firing.json")

	start := time.Now()
	var wg sync.WaitGroup
	var oks atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := doPost(t, ts.URL, "/v1/alertmanager/"+chat, payload, nil)
			if resp.StatusCode == 200 {
				oks.Add(1)
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if elapsed < 9*time.Second {
		t.Errorf("expected ≥9s for 10 calls at 1 rps, got %v", elapsed)
	}
	if oks.Load() != 10 {
		t.Errorf("expected 10 oks, got %d", oks.Load())
	}
}

func TestE2EHealthz(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
}

func TestE2EReadyzNotReady(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("readyz before getMe should be 503, got %d", resp.StatusCode)
	}
}

func TestE2EMetricsEndpoint(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServer(t, rec, 1000)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("metrics: %d", resp.StatusCode)
	}
}

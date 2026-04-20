package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/metrics"
)

func init() {
	metrics.Init()
}

func newClient(t *testing.T, server *httptest.Server, opts ...func(*Config)) Client {
	t.Helper()
	cfg := Config{
		APIURL:         server.URL,
		Token:          "test-token",
		ParseMode:      "HTML",
		RequestTimeout: 5 * time.Second,
		MaxAttempts:    3,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return New(cfg, NewLimiter(1000, 1000), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSendMessageOK(t *testing.T) {
	var got sendMessagePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/bottest-token/sendMessage") {
			t.Errorf("bad path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv)
	if err := c.SendMessage(context.Background(), 123, nil, "hi"); err != nil {
		t.Fatal(err)
	}
	if got.ChatID != 123 || got.Text != "hi" || got.ParseMode != "HTML" || !got.DisableWebPagePreview {
		t.Errorf("payload: %+v", got)
	}
}

func TestSendMessageThread(t *testing.T) {
	var got sendMessagePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	thread := 42
	c := newClient(t, srv)
	if err := c.SendMessage(context.Background(), -100, &thread, "hi"); err != nil {
		t.Fatal(err)
	}
	if got.MessageThreadID == nil || *got.MessageThreadID != 42 {
		t.Errorf("thread: %+v", got.MessageThreadID)
	}
}

func TestRetryOn429WithRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := newClient(t, srv, func(c *Config) {
		c.MaxBackoff = 20 * time.Millisecond
	})
	if err := c.SendMessage(context.Background(), 1, nil, "x"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestRetryOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, `{"ok":false,"description":"unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := newClient(t, srv)
	if err := c.SendMessage(context.Background(), 1, nil, "x"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts: %d", attempts.Load())
	}
}

func TestNoRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"ok":false,"description":"bad request"}`)
	}))
	defer srv.Close()

	c := newClient(t, srv)
	err := c.SendMessage(context.Background(), 1, nil, "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestRetryRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	c := newClient(t, srv, func(c *Config) {
		c.InitialBackoff = 100 * time.Millisecond
		c.MaxBackoff = time.Second
		c.MaxAttempts = 5
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.SendMessage(ctx, 1, nil, "x")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/getMe") {
			t.Errorf("unexpected req: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"username":"bot"}}`)
	}))
	defer srv.Close()

	c := newClient(t, srv)
	if err := c.GetMe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunSkipsCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := newClient(t, srv, func(c *Config) { c.DryRun = true })
	if err := c.SendMessage(context.Background(), 1, nil, "x"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("DRY_RUN must not call Telegram")
	}
}

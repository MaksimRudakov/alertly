package server

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

func newTestServerWithDedup(t *testing.T, rec *telegramRecorder, cache *dedup.Cache) *httptest.Server {
	t.Helper()

	registry := prometheus.NewRegistry()
	metrics.Init()

	limiter := telegram.NewLimiter(1000, 1000)
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
		Sources:   map[string]source.Source{"alertmanager": source.NewAlertmanager()},
		Renderer:  renderer,
		Telegram:  tg,
		Readiness: NewReadiness(),
		AuthToken: authToken,
		Registry:  registry,
		Dedup:     cache,
	}
	srvCfg := config.Default().Server
	s := New(srvCfg, deps)
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestDedupSuppressesDuplicateFiring(t *testing.T) {
	rec := newTelegramRecorder(t)
	cache := dedup.New(time.Hour)
	ts := newTestServerWithDedup(t, rec, cache)

	body := loadFixture(t, "alertmanager_firing.json")
	chats := "/v1/alertmanager/-1001234567890"

	resp := doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	if got := len(rec.Sent()); got != 1 {
		t.Fatalf("first call: expected 1 send, got %d", got)
	}

	// Second identical call must be suppressed.
	resp = doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	if got := len(rec.Sent()); got != 1 {
		t.Errorf("second call: expected still 1 send (deduped), got %d", got)
	}

	// Different chat → not deduped (different target).
	rec.Reset()
	resp = doPost(t, ts.URL, "/v1/alertmanager/-100999", body, nil)
	resp.Body.Close()
	if got := len(rec.Sent()); got != 1 {
		t.Errorf("different chat: expected 1 send, got %d", got)
	}
}

func TestDedupAllowsResolvedAfterFiring(t *testing.T) {
	rec := newTelegramRecorder(t)
	cache := dedup.New(time.Hour)
	ts := newTestServerWithDedup(t, rec, cache)

	chats := "/v1/alertmanager/-1001234567890"

	resp := doPost(t, ts.URL, chats, loadFixture(t, "alertmanager_firing.json"), nil)
	resp.Body.Close()

	// Resolved variant of the same fingerprint must NOT be deduped against firing.
	original := string(loadFixture(t, "alertmanager_firing.json"))
	resolved := strings.ReplaceAll(original, `"status": "firing"`, `"status": "resolved"`)
	if resolved == original {
		t.Fatal("fixture replace did not match — update the test")
	}
	resp = doPost(t, ts.URL, chats, []byte(resolved), nil)
	resp.Body.Close()

	if got := len(rec.Sent()); got != 2 {
		t.Errorf("firing+resolved: expected 2 sends (different status), got %d", got)
	}
}

func TestDedupReleasesOnSendFailure(t *testing.T) {
	// First call to Telegram fails with 400 (no retry, non-server error). Dedup
	// must be released so a follow-up retry from the caller goes through.
	var calls int
	rec := newTelegramRecorderWith(t, func(int64) (int, string) {
		calls++
		if calls == 1 {
			return 400, `{"ok":false,"description":"bad"}`
		}
		return 0, ""
	})
	cache := dedup.New(time.Hour)
	ts := newTestServerWithDedup(t, rec, cache)

	body := loadFixture(t, "alertmanager_firing.json")
	chats := "/v1/alertmanager/-1001234567890"

	resp := doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	if got := len(rec.Sent()); got != 1 {
		t.Fatalf("first call: expected 1 attempt, got %d", got)
	}

	resp = doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	if got := len(rec.Sent()); got != 2 {
		t.Errorf("retry after failure: expected 2 attempts (Forget released the key), got %d", got)
	}
}

func TestDedupNilCacheIsTransparent(t *testing.T) {
	rec := newTelegramRecorder(t)
	ts := newTestServerWithDedup(t, rec, nil)

	body := loadFixture(t, "alertmanager_firing.json")
	chats := "/v1/alertmanager/-1001234567890"

	resp := doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	resp = doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()

	if got := len(rec.Sent()); got != 2 {
		t.Errorf("nil dedup must not suppress: expected 2 sends, got %d", got)
	}
}

func TestDedupSkippedMetric(t *testing.T) {
	rec := newTelegramRecorder(t)
	cache := dedup.New(time.Hour)
	ts := newTestServerWithDedup(t, rec, cache)

	body := loadFixture(t, "alertmanager_firing.json")
	chats := "/v1/alertmanager/-1001234567890"

	before := dedupSkippedCount(t, "alertmanager", "-1001234567890", "firing")
	resp := doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	resp = doPost(t, ts.URL, chats, body, nil)
	resp.Body.Close()
	after := dedupSkippedCount(t, "alertmanager", "-1001234567890", "firing")

	if after-before != 1 {
		t.Errorf("alertly_dedup_skipped_total: delta=%v want 1", after-before)
	}
}

func dedupSkippedCount(t *testing.T, source, chatID, status string) float64 {
	t.Helper()
	mfs, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "alertly_dedup_skipped_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["source"] == source && labels["chat_id"] == chatID && labels["status"] == status {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

package server

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

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

func TestUpdatesPoller_DispatchesCallback(t *testing.T) {
	var getUpdatesCalls atomic.Int32
	var answerCalls atomic.Int32
	var silenceCalls atomic.Int32

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			n := getUpdatesCalls.Add(1)
			if n == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"callback_query":{"id":"cb1","from":{"id":42,"username":"bob"},"message":{"message_id":7,"chat":{"id":-100}},"data":"s|fp1|1h"}}]}`)
				return
			}
			// Subsequent polls return empty until timeout; short-circuit with empty.
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			answerCalls.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup"):
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer tgSrv.Close()

	amSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/alerts":
			_, _ = io.WriteString(w, `[{"fingerprint":"fp1","labels":{"alertname":"X"}}]`)
		case "/api/v2/silences":
			silenceCalls.Add(1)
			var req alertmanager.SilenceRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_, _ = io.WriteString(w, `{"silenceID":"sil-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer amSrv.Close()

	limiter := telegram.NewLimiter(1000, 1000)
	tg := telegram.New(telegram.Config{
		APIURL:         tgSrv.URL,
		Token:          "t",
		RequestTimeout: 2 * time.Second,
		MaxAttempts:    1,
	}, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)))

	am := alertmanager.New(alertmanager.Config{URL: amSrv.URL, RequestTimeout: 2 * time.Second})
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	tracker := NewButtonTracker(time.Hour)
	// Register the message that the fake Telegram server sends in the callback.
	tracker.Register(-100, 7, "fp1")
	handler := NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		AM:            am,
		Cache:         cache,
		Tracker:       tracker,
		ChatAllowlist: []int64{-100},
		Durations:     map[string]time.Duration{"1h": time.Hour},
	})

	poller := &UpdatesPoller{
		Client:      tg,
		Handler:     handler,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollTimeout: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()

	// Wait for silence to be created via callback dispatch.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if silenceCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if silenceCalls.Load() < 1 {
		t.Fatalf("expected silence to be created, got %d", silenceCalls.Load())
	}
	if answerCalls.Load() < 1 {
		t.Fatalf("expected callback to be answered, got %d", answerCalls.Load())
	}
}

func TestUpdatesPoller_ShutdownOnCtxCancel(t *testing.T) {
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow poll; return empty after 50ms so the poller can loop.
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer tgSrv.Close()

	tg := telegram.New(telegram.Config{APIURL: tgSrv.URL, Token: "t", RequestTimeout: time.Second, MaxAttempts: 1},
		telegram.NewLimiter(1000, 1000), slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := NewCallbackHandler(CallbackDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telegram: tg})
	poller := &UpdatesPoller{Client: tg, Handler: handler, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PollTimeout: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not exit within 3s of cancel")
	}
}

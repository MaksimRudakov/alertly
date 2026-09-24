package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/metrics"
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
	tracker := NewButtonTracker(time.Hour, 0)
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

func TestUpdatesPoller_DispatchesCommandMessage(t *testing.T) {
	var getUpdatesCalls atomic.Int32
	var sendCalls atomic.Int32

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if getUpdatesCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":9,"chat":{"id":-100},"from":{"id":42,"username":"bob"},"text":"/status"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			sendCalls.Add(1)
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":100}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer tgSrv.Close()

	limiter := telegram.NewLimiter(1000, 1000)
	tg := telegram.New(telegram.Config{
		APIURL:         tgSrv.URL,
		Token:          "t",
		RequestTimeout: 2 * time.Second,
		MaxAttempts:    1,
	}, limiter, slog.New(slog.NewTextHandler(io.Discard, nil)))

	readiness := NewReadiness()
	readiness.MarkReady()
	msgHandler := NewMessageHandler(CommandDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		ChatAllowlist: []int64{-100},
		Status: &StatusReporter{
			StartedAt: time.Now(),
			Version:   "test",
			Commit:    "abc",
			Readiness: readiness,
		},
	})

	poller := &UpdatesPoller{
		Client:      tg,
		Handler:     NewCallbackHandler(CallbackDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telegram: tg}),
		Messages:    msgHandler,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollTimeout: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sendCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if sendCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 status reply, got %d", sendCalls.Load())
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

// A stuck Alertmanager must not stall the poll loop: the per-callback timeout
// bounds Handle, the user still gets an answer, and the next update is
// processed promptly.
func TestUpdatesPoller_HandleTimeoutUnblocksLoop(t *testing.T) {
	var answerCalls atomic.Int32
	var secondCallbackSeen atomic.Int32

	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			var req struct {
				Offset int64 `json:"offset"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch {
			case req.Offset <= 1:
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"callback_query":{"id":"cb1","from":{"id":42},"message":{"message_id":7,"chat":{"id":-100}},"data":"s|fp1|1h"}}]}`)
			case req.Offset == 2:
				secondCallbackSeen.Add(1)
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			default:
				_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			}
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

	// AM hangs until the request context is canceled — simulates a stuck AM.
	amSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer amSrv.Close()

	tg := telegram.New(telegram.Config{APIURL: tgSrv.URL, Token: "t", RequestTimeout: 2 * time.Second, MaxAttempts: 1},
		telegram.NewLimiter(1000, 1000), slog.New(slog.NewTextHandler(io.Discard, nil)))
	am := alertmanager.New(alertmanager.Config{URL: amSrv.URL, RequestTimeout: 30 * time.Second})
	tracker := NewButtonTracker(time.Hour, 0)
	tracker.Register(-100, 7, "fp1")
	handler := NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		AM:            am,
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
		Tracker:       tracker,
		ChatAllowlist: []int64{-100},
		Durations:     map[string]time.Duration{"1h": time.Hour},
	})
	poller := &UpdatesPoller{
		Client:        tg,
		Handler:       handler,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollTimeout:   50 * time.Millisecond,
		HandleTimeout: 200 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if secondCallbackSeen.Load() >= 1 && answerCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if secondCallbackSeen.Load() < 1 {
		t.Error("poller stayed blocked on stuck AM: never advanced past the first callback")
	}
	if answerCalls.Load() < 1 {
		t.Error("user never got an answer for the timed-out callback")
	}
}

func TestDispatchRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	p := &UpdatesPoller{Logger: slog.New(slog.NewTextHandler(&logs, nil))}

	p.dispatch(context.Background(), time.Second, 7, func(context.Context) {
		panic("boom")
	})

	if !strings.Contains(logs.String(), "panic in update handler recovered") {
		t.Fatalf("panic not logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Fatalf("panic value missing from log: %s", logs.String())
	}
}

func TestUpdatesPoller_SurvivesPanickingHandler(t *testing.T) {
	var getUpdatesCalls atomic.Int32
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			if getUpdatesCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":1,"callback_query":{"id":"cb1","from":{"id":42},"message":{"message_id":7,"chat":{"id":-100}},"data":"s|fp1|1h"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer tgSrv.Close()

	tg := telegram.New(telegram.Config{
		APIURL:         tgSrv.URL,
		Token:          "t",
		RequestTimeout: 2 * time.Second,
		MaxAttempts:    1,
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A nil CallbackHandler panics on the first dereference inside Handle —
	// exactly the class of failure the guard must absorb.
	poller := &UpdatesPoller{
		Client:      tg,
		Handler:     nil,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollTimeout: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()

	// The loop must keep polling after the panicking update.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if getUpdatesCalls.Load() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := getUpdatesCalls.Load(); got < 3 {
		t.Fatalf("poller died after panic: only %d polls", got)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Telegram answers 409 when a second consumer polls the same bot token. The
// poller must name that cause instead of reporting a generic 4xx.
func TestUpdatesPoller_ConflictIsReported(t *testing.T) {
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`)
	}))
	defer tgSrv.Close()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	tg := telegram.New(telegram.Config{APIURL: tgSrv.URL, Token: "t", RequestTimeout: time.Second, MaxAttempts: 1},
		telegram.NewLimiter(1000, 1000), logger)
	poller := &UpdatesPoller{
		Client:      tg,
		Handler:     NewCallbackHandler(CallbackDeps{Logger: logger, Telegram: tg}),
		Logger:      logger,
		PollTimeout: 50 * time.Millisecond,
	}

	before := counterValue(t, "conflict")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { poller.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "getUpdates conflict") {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(logs.String(), "getUpdates conflict") {
		t.Fatalf("conflict not reported in logs:\n%s", logs.String())
	}
	if got := counterValue(t, "conflict") - before; got < 1 {
		t.Errorf("conflict metric delta = %v, want >= 1", got)
	}
}

func counterValue(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.UpdatesPollErrors.WithLabelValues(reason).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// A rolling update makes the old and new pod poll at once for a few seconds;
// that must stay a warning. Only a conflict streak longer than
// conflictPersistAfter is an error.
func TestReportConflict_EscalatesOnlyWhenPersistent(t *testing.T) {
	logs := &syncBuffer{}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	p := &UpdatesPoller{
		Logger: slog.New(slog.NewTextHandler(logs, nil)),
		now:    func() time.Time { return now },
	}
	errConflict := &telegram.APIError{StatusCode: http.StatusConflict}

	for i := 0; i < 5; i++ { // rollout overlap: a burst within seconds
		p.reportConflict(errConflict, time.Second)
		now = now.Add(2 * time.Second)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("short conflict burst must not be an error:\n%s", logs.String())
	}

	now = now.Add(conflictPersistAfter)
	p.reportConflict(errConflict, time.Second)
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "another instance is polling") {
		t.Fatalf("persistent conflict must be an error:\n%s", logs.String())
	}

	// After a quiet gap the streak restarts: next conflict is a warning again.
	before := strings.Count(logs.String(), "level=ERROR")
	now = now.Add(conflictStreakGap + time.Second)
	p.reportConflict(errConflict, time.Second)
	if strings.Count(logs.String(), "level=ERROR") != before {
		t.Errorf("conflict after a quiet gap must start a new streak:\n%s", logs.String())
	}
}

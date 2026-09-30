package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/server"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

type fakeTelegram struct {
	getMeErr atomic.Pointer[error]
}

func (f *fakeTelegram) setGetMeErr(err error) { f.getMeErr.Store(&err) }

func (f *fakeTelegram) GetMe(context.Context) error {
	if p := f.getMeErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (f *fakeTelegram) SendMessage(context.Context, int64, *int, string, *telegram.SendOptions) (int64, error) {
	return 0, nil
}
func (f *fakeTelegram) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}
func (f *fakeTelegram) AnswerCallbackQuery(context.Context, string, string, bool) error { return nil }
func (f *fakeTelegram) EditMessageText(context.Context, int64, int64, string, *telegram.InlineKeyboardMarkup) error {
	return nil
}
func (f *fakeTelegram) EditMessageReplyMarkup(context.Context, int64, int64, *telegram.InlineKeyboardMarkup) error {
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// Startup: unready while getMe fails, ready once it succeeds; the loop keeps
// probing afterwards and exits on ctx cancel.
func TestTelegramHealthLoop_StartupAndRecovery(t *testing.T) {
	fake := &fakeTelegram{}
	fake.setGetMeErr(errors.New("dial tcp: connection refused"))
	readiness := server.NewReadiness()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sinkHealthLoop(ctx, "telegram", fake.GetMe, readiness, logger, 20*time.Millisecond)
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool {
		ready, reason := readiness.IsReady()
		return !ready && reason != "" && reason != "startup: telegram getMe pending"
	}, "readiness never went unready with a getMe reason during failing startup")

	fake.setGetMeErr(nil)
	waitFor(t, 5*time.Second, func() bool {
		ready, _ := readiness.IsReady()
		return ready
	}, "readiness never recovered after getMe started succeeding")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("health loop did not exit on ctx cancel")
	}
}

// A send-failure window must not outlive the outage that caused it: once getMe
// succeeds again the pod has to become ready even though no send has succeeded
// since. Without this the state is self-locking — unready drops the pod from
// the Service endpoints, so no webhook arrives and RecordSendSuccess (the only
// other way out) can never fire.
func TestTelegramHealthLoop_ClearsSendFailureUnreadiness(t *testing.T) {
	fake := &fakeTelegram{}
	readiness := server.NewReadiness()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sinkHealthLoop(ctx, "telegram", fake.GetMe, readiness, logger, 20*time.Millisecond)

	waitFor(t, 2*time.Second, func() bool {
		ready, _ := readiness.IsReady()
		return ready
	}, "readiness never became ready while getMe succeeded")

	// Telegram goes down for outbound sends only: the send path records enough
	// server errors to trip the window, while getMe keeps succeeding.
	for i := 0; i < 10; i++ {
		readiness.RecordSendFailure(true)
	}
	if ready, reason := readiness.IsReady(); ready {
		t.Fatalf("send-failure window did not flip readiness: reason=%q", reason)
	}

	waitFor(t, 5*time.Second, func() bool {
		ready, _ := readiness.IsReady()
		return ready
	}, "readiness stayed unready although getMe kept succeeding (send-failure deadlock)")
}

func TestBuildClusters(t *testing.T) {
	cfg := config.Default()
	cfg.Alertmanager.URL = "http://am-default:9093"
	cfg.Clusters = map[string]config.Cluster{
		"k8s-prod": {
			Alias:        "prod",
			AuthTokenEnv: "TEST_TOKEN_PROD",
			Alertmanager: config.ClusterAlertmanager{URL: "http://am-prod:9093"},
			Destinations: map[string][]config.TargetSpec{"default": {{Telegram: "-100:7"}}},
		},
		"edge": {AuthTokenEnv: "TEST_TOKEN_EDGE"},
	}
	t.Setenv("TEST_TOKEN_PROD", "p")
	t.Setenv("TEST_TOKEN_EDGE", "e")

	clusters, tokens, err := buildClusters(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 3 || tokens["k8s-prod"] != "p" || tokens["edge"] != "e" {
		t.Fatalf("clusters=%d tokens=%v", len(clusters), tokens)
	}
	if _, ok := tokens[config.DefaultCluster]; ok {
		t.Error("implicit default cluster is served by WEBHOOK_AUTH_TOKEN, not a cluster token")
	}
	if clusters["k8s-prod"].AM == nil || clusters["edge"].AM != nil || clusters[config.DefaultCluster].AM == nil {
		t.Error("AM clients: only clusters with a url get one")
	}
	if tg := clusters["k8s-prod"].Destinations["default"][0]; tg.Chat != "-100" || tg.Thread != "7" {
		t.Errorf("target: %+v", tg)
	}
	if pc := pipelineCluster(clusters); pc == nil || pc.Name != config.DefaultCluster {
		t.Errorf("pipeline cluster: %+v", pc)
	}

	t.Setenv("TEST_TOKEN_EDGE", "p")
	if _, _, err := buildClusters(cfg); err == nil || !strings.Contains(err.Error(), "own token") {
		t.Errorf("shared token value must be rejected, got %v", err)
	}
	t.Setenv("TEST_TOKEN_EDGE", "")
	if _, _, err := buildClusters(cfg); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty token env must be rejected, got %v", err)
	}
}

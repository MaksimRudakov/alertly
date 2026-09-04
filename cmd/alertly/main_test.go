package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

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
		telegramHealthLoop(ctx, fake, readiness, logger, 20*time.Millisecond)
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
	go telegramHealthLoop(ctx, fake, readiness, logger, 20*time.Millisecond)

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

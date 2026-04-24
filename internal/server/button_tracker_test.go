package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/telegram"
)

func TestButtonTracker_RegisterValid(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	tr.Register(-100, 42, "fp")
	if !tr.Valid(-100, 42) {
		t.Error("expected valid")
	}
	if tr.Valid(-100, 43) {
		t.Error("different message_id must be invalid")
	}
	if tr.Valid(-999, 42) {
		t.Error("different chat_id must be invalid")
	}
}

func TestButtonTracker_RegisterZeroMessageIDIgnored(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	tr.Register(-100, 0, "fp")
	if tr.Len() != 0 {
		t.Error("message_id=0 must be ignored")
	}
}

func TestButtonTracker_Expiry(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	now := time.Unix(0, 0)
	tr.now = func() time.Time { return now }

	tr.Register(-100, 42, "fp")
	if !tr.Valid(-100, 42) {
		t.Fatal("expected valid right after register")
	}
	now = now.Add(time.Hour + time.Second)
	if tr.Valid(-100, 42) {
		t.Error("expected expired after TTL elapses")
	}
}

func TestButtonTracker_Sweep(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	now := time.Unix(0, 0)
	tr.now = func() time.Time { return now }

	tr.Register(-100, 1, "fp1")
	tr.Register(-100, 2, "fp2")
	now = now.Add(time.Hour)
	tr.Register(-100, 3, "fp3") // expires 1h after the new "now"

	now = now.Add(time.Second) // fp1, fp2 expired; fp3 still valid
	expired := tr.Sweep()
	if len(expired) != 2 {
		t.Fatalf("expected 2 expired, got %d", len(expired))
	}
	// Sweep must remove them from the tracker.
	if tr.Valid(-100, 1) || tr.Valid(-100, 2) {
		t.Error("sweep must remove entries from tracker")
	}
	if !tr.Valid(-100, 3) {
		t.Error("fp3 should remain")
	}
}

func TestButtonTracker_Consume(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	tr.Register(-100, 42, "fp")
	tr.Consume(-100, 42)
	if tr.Valid(-100, 42) {
		t.Error("consume must remove the entry")
	}
	// Idempotent:
	tr.Consume(-100, 42)
}

func TestButtonTracker_NilSafe(t *testing.T) {
	var tr *ButtonTracker
	tr.Register(1, 1, "x")
	if tr.Valid(1, 1) {
		t.Error("nil tracker must always return invalid")
	}
	if len(tr.Sweep()) != 0 {
		t.Error("nil Sweep must return empty slice")
	}
	if tr.Len() != 0 {
		t.Error("nil Len must return 0")
	}
}

// --- Sweeper tests ---------------------------------------------------------

type sweepFakeTG struct {
	mu       sync.Mutex
	calls    []edit
	editErr  error
	sleep    time.Duration
	finished atomic.Int32
}

func (f *sweepFakeTG) SendMessage(context.Context, int64, *int, string, *telegram.SendOptions) (int64, error) {
	return 0, nil
}
func (f *sweepFakeTG) GetMe(context.Context) error { return nil }
func (f *sweepFakeTG) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}
func (f *sweepFakeTG) AnswerCallbackQuery(context.Context, string, string, bool) error { return nil }
func (f *sweepFakeTG) EditMessageText(context.Context, int64, int64, string, *telegram.InlineKeyboardMarkup) error {
	return nil
}
func (f *sweepFakeTG) EditMessageReplyMarkup(_ context.Context, chatID, messageID int64, m *telegram.InlineKeyboardMarkup) error {
	if f.sleep > 0 {
		time.Sleep(f.sleep)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, edit{ChatID: chatID, MessageID: messageID, Markup: m})
	f.finished.Add(1)
	return f.editErr
}

func TestButtonSweeper_EditsExpired(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	now := time.Unix(0, 0)
	tr.now = func() time.Time { return now }
	tr.Register(-100, 1, "fp1")
	tr.Register(-100, 2, "fp2")
	now = now.Add(time.Hour + time.Second)

	tg := &sweepFakeTG{}
	sw := &ButtonSweeper{
		Tracker:  tr,
		Telegram: tg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval: time.Millisecond,
	}
	sw.sweepOnce(context.Background())
	if got := tg.finished.Load(); got != 2 {
		t.Errorf("expected 2 edits, got %d", got)
	}
	for _, e := range tg.calls {
		if e.Markup != nil {
			t.Errorf("sweeper must clear markup, got %+v", e.Markup)
		}
	}
}

func TestButtonSweeper_ContinuesOnEditError(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	now := time.Unix(0, 0)
	tr.now = func() time.Time { return now }
	tr.Register(-100, 1, "fp")
	tr.Register(-100, 2, "fp")
	now = now.Add(time.Hour + time.Second)

	tg := &sweepFakeTG{editErr: errors.New("msg gone")}
	sw := &ButtonSweeper{Tracker: tr, Telegram: tg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sw.sweepOnce(context.Background())
	if tg.finished.Load() != 2 {
		t.Errorf("expected both edits attempted even on error, got %d", tg.finished.Load())
	}
	// Tracker must have dropped them regardless of edit outcome.
	if tr.Len() != 0 {
		t.Errorf("tracker must drop expired entries; Len=%d", tr.Len())
	}
}

func TestButtonSweeper_Run_StopsOnCtxCancel(t *testing.T) {
	tr := NewButtonTracker(time.Hour)
	tg := &sweepFakeTG{}
	sw := &ButtonSweeper{
		Tracker:  tr,
		Telegram: tg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Interval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sw.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop within 1s of cancel")
	}
}

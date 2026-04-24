package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

func init() {
	metrics.Init()
}

func TestParseCallbackData(t *testing.T) {
	cases := []struct {
		in         string
		wantAct    string
		wantFP     string
		wantDur    string
		wantErr    bool
		descripion string
	}{
		{"s|abc|1h", "s", "abc", "1h", false, "normal"},
		{"s|9bcf23a1e4d08b17|24h", "s", "9bcf23a1e4d08b17", "24h", false, "fp16"},
		{"s|abc", "", "", "", true, "too few fields"},
		{"s|abc|1h|extra", "", "", "", true, "too many fields"},
		{"|abc|1h", "", "", "", true, "empty action"},
		{"s||1h", "", "", "", true, "empty fp"},
		{"s|abc|", "", "", "", true, "empty duration"},
		{"", "", "", "", true, "empty"},
	}
	for _, c := range cases {
		t.Run(c.descripion, func(t *testing.T) {
			act, fp, dur, err := ParseCallbackData(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if act != c.wantAct || fp != c.wantFP || dur != c.wantDur {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)", act, fp, dur, c.wantAct, c.wantFP, c.wantDur)
			}
		})
	}
}

func TestBuildCallbackDataSize(t *testing.T) {
	// sha256-16 fingerprint + typical duration — must stay within 64 bytes.
	data := BuildCallbackData(CallbackActionSilence, "9bcf23a1e4d08b17", "24h")
	if len(data) > 64 {
		t.Errorf("callback_data too long: %d bytes", len(data))
	}
}

// --- fake telegram client --------------------------------------------------

type fakeTG struct {
	mu              sync.Mutex
	answers         []answer
	editedMarkups   []edit
	editedTexts     []edit
	sendErr         error
	answerErr       error
	editReplyErr    error
	editTextErr     error
	callbacksCalled int
}

type answer struct {
	ID        string
	Text      string
	ShowAlert bool
}
type edit struct {
	ChatID    int64
	MessageID int64
	Text      string
	Markup    *telegram.InlineKeyboardMarkup
}

func (f *fakeTG) SendMessage(context.Context, int64, *int, string, *telegram.SendOptions) (int64, error) {
	return 0, f.sendErr
}
func (f *fakeTG) GetMe(context.Context) error { return nil }
func (f *fakeTG) GetUpdates(context.Context, int64, time.Duration) ([]telegram.Update, error) {
	return nil, nil
}
func (f *fakeTG) AnswerCallbackQuery(_ context.Context, id, text string, showAlert bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callbacksCalled++
	f.answers = append(f.answers, answer{ID: id, Text: text, ShowAlert: showAlert})
	return f.answerErr
}
func (f *fakeTG) EditMessageText(_ context.Context, chatID, messageID int64, text string, m *telegram.InlineKeyboardMarkup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editedTexts = append(f.editedTexts, edit{ChatID: chatID, MessageID: messageID, Text: text, Markup: m})
	return f.editTextErr
}
func (f *fakeTG) EditMessageReplyMarkup(_ context.Context, chatID, messageID int64, m *telegram.InlineKeyboardMarkup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editedMarkups = append(f.editedMarkups, edit{ChatID: chatID, MessageID: messageID, Markup: m})
	return f.editReplyErr
}

// --- fake AM client --------------------------------------------------------

type fakeAM struct {
	labels        map[string]map[string]string
	getErr        error
	silenceErr    error
	silenceID     string
	createdReqs   []alertmanager.SilenceRequest
	mu            sync.Mutex
	getAlertCalls int
}

func (f *fakeAM) GetAlertLabels(_ context.Context, fp string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getAlertCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	l, ok := f.labels[fp]
	if !ok {
		return nil, alertmanager.ErrAlertNotFound
	}
	return l, nil
}

func (f *fakeAM) CreateSilence(_ context.Context, req alertmanager.SilenceRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdReqs = append(f.createdReqs, req)
	if f.silenceErr != nil {
		return "", f.silenceErr
	}
	if f.silenceID == "" {
		return "silence-1", nil
	}
	return f.silenceID, nil
}

// --- tests -----------------------------------------------------------------

func newHandler(tg *fakeTG, am *fakeAM, cache *alertmanager.LabelCache, chats, users []int64) *CallbackHandler {
	// Tracker keys by (chat_id, message_id); the fingerprint stored here does
	// not need to match the one in callback_data.
	tracker := NewButtonTracker(time.Hour)
	tracker.Register(-100, 42, "any")
	return NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		AM:            am,
		Cache:         cache,
		Tracker:       tracker,
		ChatAllowlist: chats,
		UserAllowlist: users,
		Durations:     map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour},
	})
}

func mkCallback(data string, chatID int64, userID int64) *telegram.CallbackQuery {
	return &telegram.CallbackQuery{
		ID:      "cbid",
		From:    telegram.User{ID: userID, Username: "alice"},
		Message: &telegram.Message{MessageID: 42, Chat: telegram.Chat{ID: chatID}},
		Data:    data,
	}
}

func TestCallback_HappyPath_AMHasLabels(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{
		"fp1": {"alertname": "X", "severity": "warning"},
	}}
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	h := newHandler(tg, am, cache, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp1|1h", -100, 1))

	if len(am.createdReqs) != 1 {
		t.Fatalf("expected 1 silence, got %d", len(am.createdReqs))
	}
	if len(am.createdReqs[0].Matchers) != 2 {
		t.Errorf("matchers: %+v", am.createdReqs[0].Matchers)
	}
	if len(tg.editedMarkups) != 1 || tg.editedMarkups[0].Markup != nil {
		t.Errorf("expected markup stripped, got %+v", tg.editedMarkups)
	}
	if len(tg.answers) != 1 || tg.answers[0].ShowAlert {
		t.Errorf("answer: %+v", tg.answers)
	}
}

func TestCallback_FallbackToCache(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{}} // empty → not found
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	cache.Put("fp-resolved", map[string]string{"alertname": "Gone"})
	h := newHandler(tg, am, cache, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp-resolved|1h", -100, 1))

	if len(am.createdReqs) != 1 {
		t.Errorf("expected silence from cached labels, got %d", len(am.createdReqs))
	}
}

func TestCallback_RejectsUnlistedChat(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{"fp": {"a": "1"}}}
	h := newHandler(tg, am, nil, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp|1h", -999, 1))

	if len(am.createdReqs) != 0 {
		t.Error("silence must not be created from unlisted chat")
	}
	if len(tg.answers) != 1 || !tg.answers[0].ShowAlert {
		t.Errorf("expected show_alert answer, got %+v", tg.answers)
	}
}

func TestCallback_RejectsUnlistedUser(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{"fp": {"a": "1"}}}
	h := newHandler(tg, am, nil, []int64{-100}, []int64{999})

	h.Handle(context.Background(), mkCallback("s|fp|1h", -100, 1))

	if len(am.createdReqs) != 0 {
		t.Error("silence must not be created for unlisted user")
	}
}

func TestCallback_InvalidDuration(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{"fp": {"a": "1"}}}
	h := newHandler(tg, am, nil, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp|99h", -100, 1))

	if len(am.createdReqs) != 0 {
		t.Error("unsupported duration must not create silence")
	}
	if tg.answers[0].Text != "⚠️ Unsupported silence duration." {
		t.Errorf("answer: %+v", tg.answers[0])
	}
}

func TestCallback_AlertNotFound(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{}}
	cache := alertmanager.NewLabelCache(time.Hour, 10)
	h := newHandler(tg, am, cache, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp-unknown|1h", -100, 1))

	if len(am.createdReqs) != 0 {
		t.Error("silence must not be created when alert is unknown")
	}
}

func TestCallback_AMErrorPropagates(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{
		labels: map[string]map[string]string{"fp": {"a": "1"}},
		silenceErr: &alertmanager.APIError{
			StatusCode: 400, Body: "bad matcher",
		},
	}
	h := newHandler(tg, am, nil, []int64{-100}, nil)

	h.Handle(context.Background(), mkCallback("s|fp|1h", -100, 1))

	if len(tg.editedMarkups) != 0 {
		t.Error("markup must not be stripped when silence failed")
	}
	if len(tg.answers) != 1 || !tg.answers[0].ShowAlert {
		t.Error("expected error answer with show_alert")
	}
}

func TestCallback_GetLabelsErrorPropagates(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{getErr: errors.New("boom")}
	h := newHandler(tg, am, nil, []int64{-100}, nil)
	h.Handle(context.Background(), mkCallback("s|fp|1h", -100, 1))
	if len(am.createdReqs) != 0 {
		t.Error("silence must not be created if GetAlertLabels errors")
	}
}

func TestCallback_InvalidDataNoSilence(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{}
	h := newHandler(tg, am, nil, []int64{-100}, nil)
	h.Handle(context.Background(), mkCallback("garbage", -100, 1))
	if len(am.createdReqs) != 0 {
		t.Error("silence must not be created for invalid data")
	}
	if am.getAlertCalls != 0 {
		t.Error("AM must not be queried for invalid data")
	}
}

func TestCallback_ExpiredWindow(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{"fp": {"a": "1"}}}
	// Build a handler with an empty tracker — simulating expired / unknown message.
	tracker := NewButtonTracker(time.Hour)
	h := NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		AM:            am,
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
		Tracker:       tracker,
		ChatAllowlist: []int64{-100},
		Durations:     map[string]time.Duration{"1h": time.Hour},
	})
	h.Handle(context.Background(), mkCallback("s|fp|1h", -100, 1))

	if len(am.createdReqs) != 0 {
		t.Error("expired window must not create silence")
	}
	if len(tg.editedMarkups) != 1 || tg.editedMarkups[0].Markup != nil {
		t.Errorf("expected keyboard strip, got %+v", tg.editedMarkups)
	}
	if len(tg.answers) != 1 || !tg.answers[0].ShowAlert {
		t.Error("expected show_alert toast")
	}
}

func TestCallback_ConsumeOnSuccess(t *testing.T) {
	tg := &fakeTG{}
	am := &fakeAM{labels: map[string]map[string]string{"fp": {"a": "1"}}}
	tracker := NewButtonTracker(time.Hour)
	tracker.Register(-100, 42, "fp")

	h := NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		AM:            am,
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
		Tracker:       tracker,
		ChatAllowlist: []int64{-100},
		Durations:     map[string]time.Duration{"1h": time.Hour},
	})
	h.Handle(context.Background(), mkCallback("s|fp|1h", -100, 1))

	if tracker.Valid(-100, 42) {
		t.Error("tracker entry must be consumed after successful silence")
	}
}

func TestSilenceCreatedBy(t *testing.T) {
	if got := silenceCreatedBy(telegram.User{ID: 5, Username: "bob"}); got != "telegram:@bob" {
		t.Errorf("with username: %q", got)
	}
	if got := silenceCreatedBy(telegram.User{ID: 5}); got != "telegram:5" {
		t.Errorf("without username: %q", got)
	}
}

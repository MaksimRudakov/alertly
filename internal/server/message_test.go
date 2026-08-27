package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/status", "status"},
		{"/status@alertly_bot", "status"},
		{"/STATUS extra args", "status"},
		{"/alerts", "alerts"},
		{"plain text", ""},
		{"", ""},
		{"  ", ""},
		{"not /a command", ""},
	}
	for _, c := range cases {
		if got := parseCommand(c.in); got != c.want {
			t.Errorf("parseCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func newStatusReporter() *StatusReporter {
	r := NewReadiness()
	r.MarkReady()
	return &StatusReporter{
		StartedAt: time.Now().Add(-90 * time.Second),
		Version:   "test",
		Commit:    "abc123",
		Readiness: r,
		Sizes:     []SizeStat{{Label: "Dedup cache", Len: func() int { return 7 }}},
	}
}

func newMessageHandler(tg *fakeTG, chats, users []int64) *MessageHandler {
	return NewMessageHandler(CommandDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		ChatAllowlist: chats,
		UserAllowlist: users,
		Status:        newStatusReporter(),
	})
}

func statusMsg(chatID, userID int64) *telegram.Message {
	return &telegram.Message{
		MessageID: 10,
		Chat:      telegram.Chat{ID: chatID},
		From:      &telegram.User{ID: userID, Username: "oncall"},
		Text:      "/status",
	}
}

func TestMessageHandlerStatusOK(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, nil)

	h.Handle(context.Background(), statusMsg(-100, 42))

	if len(tg.sentMessages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(tg.sentMessages))
	}
	got := tg.sentMessages[0]
	if got.ChatID != -100 {
		t.Errorf("sent to chat %d, want -100", got.ChatID)
	}
	for _, want := range []string{"alertly status", "test", "abc123", "Ready: ✅", "Dedup cache: 7"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("status text missing %q:\n%s", want, got.Text)
		}
	}
}

func TestMessageHandlerUnreadyReasonEscaped(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, nil)
	h.deps.Status.Readiness.MarkUnready("telegram <b>down</b>")

	h.Handle(context.Background(), statusMsg(-100, 42))

	if len(tg.sentMessages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(tg.sentMessages))
	}
	text := tg.sentMessages[0].Text
	if !strings.Contains(text, "Ready: ❌") || strings.Contains(text, "<b>down</b>") {
		t.Errorf("unready reason not present or not escaped:\n%s", text)
	}
}

func TestMessageHandlerChatNotAllowed(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, nil)

	h.Handle(context.Background(), statusMsg(-200, 42))

	if len(tg.sentMessages) != 0 {
		t.Fatalf("expected silence for non-allowlisted chat, got %d messages", len(tg.sentMessages))
	}
}

func TestMessageHandlerUserNotAllowed(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, []int64{1})

	h.Handle(context.Background(), statusMsg(-100, 42))

	if len(tg.sentMessages) != 1 {
		t.Fatalf("expected 1 rejection message, got %d", len(tg.sentMessages))
	}
	if !strings.Contains(tg.sentMessages[0].Text, "not authorized") {
		t.Errorf("expected rejection text, got: %s", tg.sentMessages[0].Text)
	}
}

func TestMessageHandlerIgnoresUnknownAndPlainText(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, nil)

	for _, text := range []string{"/silence 1h", "/start", "hello there", ""} {
		msg := statusMsg(-100, 42)
		msg.Text = text
		h.Handle(context.Background(), msg)
	}

	if len(tg.sentMessages) != 0 {
		t.Fatalf("expected no replies, got %d", len(tg.sentMessages))
	}
}

func TestMessageHandlerRepliesIntoTopicThread(t *testing.T) {
	tg := &fakeTG{}
	h := newMessageHandler(tg, []int64{-100}, nil)

	thread := 42
	msg := statusMsg(-100, 42)
	msg.MessageThreadID = &thread
	msg.IsTopicMessage = true
	h.Handle(context.Background(), msg)

	if len(tg.sentMessages) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(tg.sentMessages))
	}
	if got := tg.sentMessages[0].ThreadID; got == nil || *got != thread {
		t.Errorf("expected reply into thread %d, got %v", thread, got)
	}

	// Reply-chain thread id in a regular group must NOT be treated as a topic.
	tg2 := &fakeTG{}
	h2 := newMessageHandler(tg2, []int64{-100}, nil)
	msg2 := statusMsg(-100, 42)
	msg2.MessageThreadID = &thread
	h2.Handle(context.Background(), msg2)
	if got := tg2.sentMessages[0].ThreadID; got != nil {
		t.Errorf("expected nil thread for non-topic message, got %v", got)
	}
}

func TestMessageHandlerSendError(t *testing.T) {
	tg := &fakeTG{sendErr: errors.New("boom")}
	h := newMessageHandler(tg, []int64{-100}, nil)

	// Must not panic and must not retry beyond the client's own logic.
	h.Handle(context.Background(), statusMsg(-100, 42))
}

func pipelineReporter(am *fakeAM) *StatusReporter {
	r := newStatusReporter()
	r.AM = am
	r.Pipeline = PipelineConfig{Enabled: true, WatchdogAlert: "Watchdog", Timeout: time.Second}
	r.Activity = NewActivityTracker()
	return r
}

func TestStatusPipelineHealthy(t *testing.T) {
	am := &fakeAM{
		statusInfo: alertmanager.StatusInfo{Version: "0.27.0", ClusterStatus: "ready"},
		overview: alertmanager.AlertsOverview{
			Firing: 3, Silenced: 1,
			WatchdogSeen: true, WatchdogUpdatedAt: time.Now().Add(-30 * time.Second),
		},
	}
	r := pipelineReporter(am)
	r.Activity.RecordWebhook("alertmanager")
	r.Activity.RecordDelivery()

	text := r.Text(context.Background())
	for _, want := range []string{
		"Alertmanager: ✅ v0.27.0, cluster ready",
		"Watchdog: ✅ updated 30s ago",
		"Alerts in AM: 3 firing, 1 silenced",
		"Last webhook: 0s ago (alertmanager)",
		"Last delivery: 0s ago",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestStatusPipelineAMDown(t *testing.T) {
	am := &fakeAM{statusErr: errors.New("dial tcp 10.0.0.1:9093: connect: connection refused")}
	text := pipelineReporter(am).Text(context.Background())
	if !strings.Contains(text, "Alertmanager: ❌ unreachable") || !strings.Contains(text, "skipped (AM unreachable)") {
		t.Errorf("expected AM-down diagnostics in:\n%s", text)
	}
}

func TestStatusPipelinePrometheusDead(t *testing.T) {
	// AM alive but no Watchdog — the deadman check must point at Prometheus.
	am := &fakeAM{
		statusInfo: alertmanager.StatusInfo{Version: "0.27.0", ClusterStatus: "ready"},
		overview:   alertmanager.AlertsOverview{Firing: 0, Silenced: 0, WatchdogSeen: false},
	}
	text := pipelineReporter(am).Text(context.Background())
	if !strings.Contains(text, "Watchdog: ❌") || !strings.Contains(text, "check Prometheus") {
		t.Errorf("expected missing-watchdog diagnostics in:\n%s", text)
	}

	// Stale Watchdog (AM keeps it, Prometheus stopped refreshing).
	am.overview = alertmanager.AlertsOverview{
		WatchdogSeen: true, WatchdogUpdatedAt: time.Now().Add(-30 * time.Minute),
	}
	text = pipelineReporter(am).Text(context.Background())
	if !strings.Contains(text, "Watchdog: ⚠️ stale") {
		t.Errorf("expected stale-watchdog diagnostics in:\n%s", text)
	}
}

func TestStatusPipelineDisabled(t *testing.T) {
	r := newStatusReporter() // no AM, Pipeline zero-value
	if strings.Contains(r.Text(context.Background()), "Pipeline") {
		t.Error("pipeline section must be absent when disabled")
	}
}

func TestStatusNeverSeenActivity(t *testing.T) {
	am := &fakeAM{statusInfo: alertmanager.StatusInfo{Version: "0.27.0"}}
	text := pipelineReporter(am).Text(context.Background())
	if !strings.Contains(text, "Last webhook: none since start") || !strings.Contains(text, "Last delivery: none since start") {
		t.Errorf("expected never-seen activity lines in:\n%s", text)
	}
}

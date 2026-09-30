package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/slack"
)

type fakeSlackClient struct {
	mu         sync.Mutex
	ephemerals []string
	replies    []slack.CommandReply
}

func (f *fakeSlackClient) PostMessage(context.Context, slack.Message) (string, error) {
	return "1.1", nil
}
func (f *fakeSlackClient) UpdateMessage(context.Context, string, slack.Message) error { return nil }
func (f *fakeSlackClient) PostEphemeral(_ context.Context, _, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ephemerals = append(f.ephemerals, text)
	return nil
}
func (f *fakeSlackClient) Respond(_ context.Context, _ string, r slack.CommandReply) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, r)
	return nil
}
func (f *fakeSlackClient) AuthTest(context.Context) error { return nil }

type slackEnv struct {
	si      *SlackInteractive
	client  *fakeSlackClient
	sk      *recordingSink
	tracker *ButtonTracker
	undo    *ButtonTracker
	prodAM  *fakeAM
}

func newSlackEnv(t *testing.T) *slackEnv {
	t.Helper()
	e := &slackEnv{
		client:  &fakeSlackClient{},
		sk:      &recordingSink{name: sink.Slack},
		tracker: NewButtonTracker(time.Hour, 0),
		undo:    NewButtonTracker(5*time.Minute, 0),
		prodAM: &fakeAM{
			labels:     map[string]map[string]string{"fp1": {"alertname": "X"}},
			silenceID:  "sil-9",
			statusInfo: alertmanager.StatusInfo{Version: "0.28.1", ClusterStatus: "ready"},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	prod := &Cluster{Name: "k8s-prod", Alias: "prod", AM: e.prodAM, WatchdogAlert: "",
		Destinations: map[string][]sink.Target{"default": {{Sink: sink.Slack, Chat: "C0PROD0001"}}, "shared": {{Sink: sink.Slack, Chat: "C0SHARED01"}}}}
	data := &Cluster{Name: "k8s-data", Alias: "data", AM: &fakeAM{statusInfo: alertmanager.StatusInfo{Version: "0.28.1"}},
		Destinations: map[string][]sink.Target{"default": {{Sink: sink.Slack, Chat: "C0DATA0001"}}, "shared": {{Sink: sink.Slack, Chat: "C0SHARED01"}}}}
	access := map[string]AccessPolicy{sink.Slack: {Chats: []string{"C0PROD0001", "C0DATA0001", "C0SHARED01"}}}
	status := &StatusReporter{
		StartedAt: time.Now(), Version: "v0.8.0", Commit: "abc",
		Sinks:    NewSinkReadiness(map[string]ReadinessTracker{sink.Slack: readyTracker(), sink.Telegram: readyTracker()}),
		Pipeline: PipelineConfig{Enabled: true, Timeout: time.Second},
	}
	e.si = &SlackInteractive{
		Client: e.client,
		Callbacks: NewCallbackHandler(CallbackDeps{
			Logger:      logger,
			Clusters:    map[string]*Cluster{"prod": prod, "data": data},
			Cache:       alertmanager.NewLabelCache(time.Hour, 10),
			Tracker:     e.tracker,
			UndoTracker: e.undo,
			Access:      access,
			Durations:   map[string]time.Duration{"1h": time.Hour},
			Sinks:       map[string]sink.Sink{sink.Slack: e.sk},
		}),
		Commands: NewMessageHandler(CommandDeps{
			Logger:   logger,
			Access:   access,
			Status:   status,
			Clusters: map[string]*Cluster{"k8s-prod": prod, "k8s-data": data},
		}),
		Command: "/alertly",
		Logger:  logger,
	}
	return e
}

func readyTracker() ReadinessTracker {
	r := NewReadiness()
	r.MarkReady()
	return r
}

func envelope(t *testing.T, typ string, payload any) slack.Envelope {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return slack.Envelope{EnvelopeID: "e", Type: typ, Payload: b}
}

func blockAction(channel, ts, value string) map[string]any {
	return map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U0ONCALL01", "username": "oncall"},
		"channel": map[string]any{"id": channel},
		"message": map[string]any{"ts": ts, "text": "fallback", "attachments": []any{}},
		"actions": []any{map[string]any{"action_id": "alertly_0", "block_id": slack.ActionsBlockID, "value": value}},
	}
}

func TestSlackInteractive_SilenceAndUndo(t *testing.T) {
	e := newSlackEnv(t)
	ref := sink.MessageRef{Sink: sink.Slack, Chat: "C0PROD0001", ID: "1712345678.000100"}
	sent := sink.Part{Text: "fallback", Payload: json.RawMessage(`{"attachments":[{"blocks":[{"type":"header"}]}]}`)}
	e.tracker.RegisterRef("k8s-prod", ref, "fp1", sent)

	e.si.dispatch(context.Background(), envelope(t, "interactive", blockAction("C0PROD0001", ref.ID, "s|prod|fp1|1h")))

	if len(e.prodAM.createdReqs) != 1 {
		t.Fatalf("expected silence in prod AM, got %d", len(e.prodAM.createdReqs))
	}
	if by := e.prodAM.createdReqs[0].CreatedBy; by != "slack:@oncall" {
		t.Errorf("createdBy: %q", by)
	}
	ups := e.sk.Updates()
	if len(ups) != 1 || ups[0].Actions == nil || ups[0].Actions.Rows[0][0].Data != "u|prod|sil-9|-" {
		t.Fatalf("expected undo button via chat.update, got %+v", ups)
	}
	if string(ups[0].Original.Payload) != string(sent.Payload) {
		t.Error("chat.update must resend the message as originally sent")
	}
	if len(e.client.ephemerals) != 1 || !strings.Contains(e.client.ephemerals[0], "Silenced 1h") {
		t.Errorf("ephemeral feedback: %v", e.client.ephemerals)
	}

	e.si.dispatch(context.Background(), envelope(t, "interactive", blockAction("C0PROD0001", ref.ID, "u|prod|sil-9|-")))
	if len(e.prodAM.deletedIDs) != 1 || e.prodAM.deletedIDs[0] != "sil-9" {
		t.Fatalf("undo must delete sil-9, got %v", e.prodAM.deletedIDs)
	}
	if last := e.sk.Updates()[len(e.sk.Updates())-1]; last.Actions != nil {
		t.Errorf("undo must remove the buttons, got %+v", last.Actions)
	}
}

func TestSlackInteractive_RejectsUnlistedChannelAndForeignButtons(t *testing.T) {
	e := newSlackEnv(t)
	e.si.dispatch(context.Background(), envelope(t, "interactive", blockAction("C0OTHER001", "1.1", "s|prod|fp1|1h")))
	if len(e.prodAM.createdReqs) != 0 || !strings.Contains(e.client.ephemerals[0], "cannot silence") {
		t.Fatalf("unlisted channel must be refused: %v", e.client.ephemerals)
	}

	// A button that is not alertly's (other block) is ignored entirely.
	p := blockAction("C0PROD0001", "1.1", "s|prod|fp1|1h")
	p["actions"] = []any{map[string]any{"block_id": "someone_else", "value": "s|prod|fp1|1h"}}
	e.si.dispatch(context.Background(), envelope(t, "interactive", p))
	if len(e.client.ephemerals) != 1 {
		t.Errorf("foreign block must be ignored, got %v", e.client.ephemerals)
	}
}

func TestSlackInteractive_StatusCommand(t *testing.T) {
	e := newSlackEnv(t)
	cmd := func(channel, text string) {
		e.si.dispatch(context.Background(), envelope(t, "slash_commands", map[string]any{
			"command": "/alertly", "text": text, "channel_id": channel,
			"user_id": "U0ONCALL01", "user_name": "oncall", "response_url": "https://hooks.slack.com/x",
		}))
	}

	cmd("C0PROD0001", "status")
	r := e.client.replies[0]
	if r.ResponseType != "in_channel" || !strings.Contains(r.Text, "*Pipeline — k8s-prod*") || strings.Contains(r.Text, "k8s-data") {
		t.Fatalf("status must cover the clusters routed to this channel only:\n%s", r.Text)
	}
	if !strings.Contains(r.Text, "Slack: ✅") || !strings.Contains(r.Text, "Telegram: ✅") {
		t.Errorf("per-sink readiness missing:\n%s", r.Text)
	}

	cmd("C0SHARED01", "status data")
	if r := e.client.replies[1]; !strings.Contains(r.Text, "k8s-data") || strings.Contains(r.Text, "k8s-prod") {
		t.Errorf("named cluster (alias) selection among the channel's clusters:\n%s", r.Text)
	}

	// k8s-data exists but does not route to C0PROD0001: indistinguishable
	// from an unknown cluster.
	cmd("C0PROD0001", "status data")
	if r := e.client.replies[2]; r.ResponseType != "ephemeral" || !strings.Contains(r.Text, "Unknown cluster") {
		t.Errorf("cluster not routed to this channel must be refused like an unknown one: %+v", r)
	}

	cmd("C0PROD0001", "silence all")
	if r := e.client.replies[3]; r.ResponseType != "ephemeral" || !strings.Contains(r.Text, "Usage") {
		t.Errorf("unknown subcommand must show usage: %+v", r)
	}

	cmd("C0OTHER001", "status")
	if r := e.client.replies[4]; r.ResponseType != "ephemeral" || !strings.Contains(r.Text, "not allowed") {
		t.Errorf("unlisted channel must get an ephemeral refusal: %+v", r)
	}

	e.si.Command = "/other"
	cmd("C0PROD0001", "status")
	if len(e.client.replies) != 5 {
		t.Error("a slash command of another app must be ignored")
	}
}

func TestSlackInteractive_PanicIsRecovered(t *testing.T) {
	e := newSlackEnv(t)
	e.si.Callbacks = nil
	e.si.Commands = nil
	e.si.Client = nil // Deny through a nil client panics
	e.si.dispatch(context.Background(), envelope(t, "slash_commands", map[string]any{"command": "/alertly", "text": "status"}))
}

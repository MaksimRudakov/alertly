package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
)

// fakeSocketMode serves apps.connections.open and a WebSocket endpoint that
// runs script for each connection.
type fakeSocketMode struct {
	srv    *httptest.Server
	opens  atomic.Int32
	auth   atomic.Value
	script func(ctx context.Context, conn *websocket.Conn, n int32)
}

func newFakeSocketMode(t *testing.T, script func(ctx context.Context, conn *websocket.Conn, n int32)) *fakeSocketMode {
	f := &fakeSocketMode{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apps.connections.open":
			n := f.opens.Add(1)
			f.auth.Store(r.Header.Get("Authorization"))
			wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/ws?n=" + string(rune('0'+n))
			_, _ = io.WriteString(w, `{"ok":true,"url":"`+wsURL+`"}`)
		case "/ws":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			f.script(r.Context(), conn, f.opens.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	return conn.Write(ctx, websocket.MessageText, b)
}

func TestSocket_AcksBeforeHandlingAndReconnectsOnRefresh(t *testing.T) {
	acks := make(chan string, 4)
	f := newFakeSocketMode(t, func(ctx context.Context, conn *websocket.Conn, n int32) {
		_ = writeJSON(ctx, conn, map[string]any{"type": "hello"})
		if n == 1 {
			_ = writeJSON(ctx, conn, map[string]any{"envelope_id": "env-1", "type": "slash_commands", "payload": map[string]any{"command": "/alertly"}})
			_, data, err := conn.Read(ctx)
			if err == nil {
				acks <- string(data)
			}
			_ = writeJSON(ctx, conn, map[string]any{"type": "disconnect", "reason": "refresh_requested"})
			_, _, _ = conn.Read(ctx) // until the client closes
			return
		}
		_ = writeJSON(ctx, conn, map[string]any{"envelope_id": "env-2", "type": "interactive", "payload": map[string]any{}})
		_, data, err := conn.Read(ctx)
		if err == nil {
			acks <- string(data)
		}
		<-ctx.Done()
	})

	var mu sync.Mutex
	var handled []string
	gotBoth := make(chan struct{})
	s := NewSocket(SocketConfig{APIURL: f.srv.URL, AppToken: "xapp-test", Logger: discard})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, func(_ context.Context, env Envelope) {
			mu.Lock()
			handled = append(handled, env.EnvelopeID+"/"+env.Type)
			if len(handled) == 2 {
				close(gotBoth)
			}
			mu.Unlock()
		})
		close(done)
	}()

	select {
	case <-gotBoth:
	case <-time.After(5 * time.Second):
		t.Fatalf("envelopes not handled; handled=%v opens=%d", handled, f.opens.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("socket did not stop on ctx cancel")
	}

	if f.opens.Load() != 2 {
		t.Errorf("refresh_requested must reconnect: opens=%d", f.opens.Load())
	}
	if got := f.auth.Load(); got != "Bearer xapp-test" {
		t.Errorf("app token header: %v", got)
	}
	var gotAcks []string
	for len(gotAcks) < 2 {
		select {
		case a := <-acks:
			gotAcks = append(gotAcks, a)
		case <-time.After(2 * time.Second):
			t.Fatalf("acks received: %v", gotAcks)
		}
	}
	if len(gotAcks) != 2 || !strings.Contains(gotAcks[0], `"envelope_id":"env-1"`) || !strings.Contains(gotAcks[1], "env-2") {
		t.Errorf("acks: %v", gotAcks)
	}
	if handled[0] != "env-1/slash_commands" || handled[1] != "env-2/interactive" {
		t.Errorf("handled: %v", handled)
	}
}

func TestSocket_OpenErrorRetries(t *testing.T) {
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		opens.Add(1)
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer srv.Close()
	s := NewSocket(SocketConfig{APIURL: srv.URL, AppToken: "xapp-bad", Logger: discard})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s.Run(ctx, func(context.Context, Envelope) { t.Error("no envelope expected") })
	if n := opens.Load(); n < 1 || n > 2 {
		t.Errorf("open attempts within 1.5s with 1s backoff: %d", n)
	}
}

func TestSink_ActionsAndSetActions(t *testing.T) {
	f := newFakeSlack(t, nil)
	s := NewSink(f.client(1), nil, true, nil)
	parts, _ := s.Render("x", notification.Notification{Title: "T", Severity: "critical", Status: "firing"})
	actions := &sink.Actions{Rows: [][]sink.Button{{{Text: "🔇 Silence 1h", Data: "s|prod|fp|1h"}}}}

	if _, err := s.Send(context.Background(), sink.Target{Sink: sink.Slack, Chat: "C0123ABCD"}, parts[0], actions); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.last)
	if !strings.Contains(string(raw), `"block_id":"alertly_actions"`) || !strings.Contains(string(raw), `"value":"s|prod|fp|1h"`) {
		t.Fatalf("actions block missing: %s", raw)
	}

	ref := sink.MessageRef{Sink: sink.Slack, Chat: "C0123ABCD", ID: "1.1"}
	if err := s.SetActions(context.Background(), ref, parts[0], nil); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(f.last)
	if f.last["ts"] != "1.1" || strings.Contains(string(raw), "alertly_actions") || !strings.Contains(string(raw), `"type":"header"`) {
		t.Errorf("chat.update must resend the body without buttons: %s", raw)
	}
}

func TestOriginalPart_StripsActions(t *testing.T) {
	msg, _ := message("C1", sink.Part{Text: "t", Payload: mustPayload(t)}, &sink.Actions{Rows: [][]sink.Button{{{Text: "b", Data: "d"}}}})
	p := OriginalPart("t", msg.Blocks)
	if strings.Contains(string(p.Payload), ActionsBlockID) || !strings.Contains(string(p.Payload), "header") {
		t.Errorf("original part: %s", p.Payload)
	}
}

func mustPayload(t *testing.T) json.RawMessage {
	t.Helper()
	parts := renderBuiltin(notification.Notification{Title: "T"}, nil)
	b, err := json.Marshal(parts[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRespond_OnlySlackHosts(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	strict := New(Config{Token: "t"}, nil, discard)
	if err := strict.Respond(context.Background(), srv.URL+"/hook", CommandReply{Text: "x"}); err == nil {
		t.Fatal("response_url outside slack.com must be refused")
	}

	host := strings.TrimPrefix(srv.URL, "http://")
	c := New(Config{Token: "t", ResponseURLHosts: []string{host}}, nil, discard)
	if err := c.Respond(context.Background(), srv.URL+"/hook", CommandReply{ResponseType: "in_channel", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if got["response_type"] != "in_channel" || got["text"] != "hi" {
		t.Errorf("reply body: %v", got)
	}
}

// A silently dropped network leaves the read blocked with no FIN/RST: the
// ping must notice the missing pong and reconnect.
func TestSocket_ReconnectsWhenPongsStop(t *testing.T) {
	f := newFakeSocketMode(t, func(ctx context.Context, conn *websocket.Conn, n int32) {
		_ = writeJSON(ctx, conn, map[string]any{"type": "hello"})
		<-ctx.Done() // never read again: pings go unanswered
	})
	s := NewSocket(SocketConfig{APIURL: f.srv.URL, AppToken: "xapp", Logger: discard,
		PingInterval: 100 * time.Millisecond, PingTimeout: 200 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, func(context.Context, Envelope) {}); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for f.opens.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if f.opens.Load() < 2 {
		t.Fatalf("dead connection not detected: opens=%d", f.opens.Load())
	}
}

// A healthy peer answers pings; the connection must stay up.
func TestSocket_KeepsConnectionWhilePongsArrive(t *testing.T) {
	f := newFakeSocketMode(t, func(ctx context.Context, conn *websocket.Conn, n int32) {
		_ = writeJSON(ctx, conn, map[string]any{"type": "hello"})
		_, _, _ = conn.Read(ctx) // reading answers pings
	})
	s := NewSocket(SocketConfig{APIURL: f.srv.URL, AppToken: "xapp", Logger: discard,
		PingInterval: 50 * time.Millisecond, PingTimeout: 500 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	s.Run(ctx, func(context.Context, Envelope) {})
	if n := f.opens.Load(); n != 1 {
		t.Errorf("healthy connection must not be recycled: opens=%d", n)
	}
}

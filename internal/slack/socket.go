package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/MaksimRudakov/alertly/internal/metrics"
)

// Envelope is one Socket Mode message.
type Envelope struct {
	EnvelopeID   string          `json:"envelope_id"`
	Type         string          `json:"type"` // hello | disconnect | interactive | slash_commands | events_api
	Payload      json.RawMessage `json:"payload"`
	RetryAttempt int             `json:"retry_attempt"`
	Reason       string          `json:"reason"` // disconnect only
}

// SocketConfig configures the Socket Mode connection.
type SocketConfig struct {
	APIURL   string // https://slack.com/api
	AppToken string // xapp-, scope connections:write
	Logger   *slog.Logger
	// QueueSize bounds envelopes waiting for the handler; beyond it new ones
	// are dropped (already acked) with a warning.
	QueueSize int
}

// Socket keeps one Socket Mode connection alive and hands envelopes to a
// handler. Envelopes are acked on receipt, before handling: Slack wants the
// ack within 3 s and redelivers otherwise, while alertly's handlers call
// Alertmanager and may take longer. Handling runs on one worker goroutine so
// the read loop keeps answering pings.
type Socket struct {
	cfg  SocketConfig
	http *http.Client
}

func NewSocket(cfg SocketConfig) *Socket {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 64
	}
	return &Socket{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// Run connects, reconnects on disconnect/errors with backoff (1 s → 30 s)
// and returns when ctx is done. handle is called sequentially.
func (s *Socket) Run(ctx context.Context, handle func(context.Context, Envelope)) {
	log := s.cfg.Logger
	queue := make(chan Envelope, s.cfg.QueueSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-queue:
				handle(ctx, env)
			}
		}
	}()
	defer func() { <-done }()

	log.Info("slack socket mode started")
	defer log.Info("slack socket mode stopped")

	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for ctx.Err() == nil {
		wsURL, err := s.open(ctx)
		var reason string
		if err != nil {
			reason = "open_error"
			log.Warn("slack socket: apps.connections.open failed", "err", err, "backoff", backoff)
		} else {
			reason = s.serve(ctx, wsURL, queue, func() { backoff = time.Second })
		}
		if ctx.Err() != nil {
			return
		}
		metrics.SlackSocketReconnects.WithLabelValues(reason).Inc()
		if reason == "refresh_requested" || reason == "warning" {
			// Planned rotation: reconnect right away.
			log.Info("slack socket: reconnecting", "reason", reason)
			continue
		}
		if reason == "link_disabled" {
			log.Error("slack socket: Socket Mode disabled for the app; enable it in the app settings")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (s *Socket) open(ctx context.Context) (string, error) {
	endpoint, err := url.JoinPath(s.cfg.APIURL, "apps.connections.open")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.cfg.AppToken)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", errors.New("apps.connections.open: request failed") // keep the app token and URL out of logs
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("apps.connections.open: status %d", resp.StatusCode)
	}
	if !r.OK || r.URL == "" {
		return "", &APIError{StatusCode: resp.StatusCode, Code: r.Error}
	}
	return r.URL, nil
}

// serve reads one connection until it ends and returns the reason.
func (s *Socket) serve(ctx context.Context, wsURL string, queue chan<- Envelope, connected func()) string {
	log := s.cfg.Logger
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		log.Warn("slack socket: dial failed", "err", errors.Unwrap(err))
		return "dial_error"
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "shutdown")
				return "shutdown"
			}
			log.Warn("slack socket: read failed", "err", err)
			return "error"
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Warn("slack socket: undecodable message", "err", err)
			continue
		}
		switch env.Type {
		case "hello":
			connected()
			log.Info("slack socket: connected")
			continue
		case "disconnect":
			_ = conn.Close(websocket.StatusNormalClosure, "")
			if env.Reason == "" {
				return "disconnect"
			}
			return env.Reason
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := conn.Write(wctx, websocket.MessageText, ack)
			cancel()
			if err != nil {
				log.Warn("slack socket: ack failed", "err", err)
				return "error"
			}
		}
		select {
		case queue <- env:
		default:
			log.Warn("slack socket: handler queue full, envelope dropped", "type", env.Type)
		}
	}
}

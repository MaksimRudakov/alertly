// Package slack is the Slack sink: a small Web API client (chat.postMessage,
// auth.test) on net/http and the Block Kit rendering of notifications.
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
	"strconv"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/metrics"
)

type Config struct {
	APIURL         string
	Token          string // bot token, xoxb-
	RequestTimeout time.Duration
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	DryRun         bool
}

// Client is the subset of the Slack Web API alertly uses.
type Client interface {
	// PostMessage posts to a channel (optionally into a thread) and returns
	// the message ts.
	PostMessage(ctx context.Context, msg Message) (string, error)
	AuthTest(ctx context.Context) error
}

// Message is a chat.postMessage request.
type Message struct {
	Channel     string            `json:"channel"`
	Text        string            `json:"text"`
	ThreadTS    string            `json:"thread_ts,omitempty"`
	Blocks      []json.RawMessage `json:"blocks,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
	UnfurlLinks bool              `json:"unfurl_links"`
	UnfurlMedia bool              `json:"unfurl_media"`
}

// Attachment carries the severity colour bar around Block Kit blocks.
type Attachment struct {
	Color    string            `json:"color,omitempty"`
	Blocks   []json.RawMessage `json:"blocks,omitempty"`
	Fallback string            `json:"fallback,omitempty"`
}

// APIError is a failed Web API call: an HTTP error status, or HTTP 200 with
// {"ok":false,"error":"<code>"}.
type APIError struct {
	StatusCode int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("slack api: status=%d error=%s", e.StatusCode, e.Code)
	}
	return fmt.Sprintf("slack api: status=%d", e.StatusCode)
}

// Slack reports some server-side failures as ok:false with HTTP 200.
var serverSideCodes = map[string]bool{
	"internal_error":      true,
	"fatal_error":         true,
	"service_unavailable": true,
	"request_timeout":     true,
}

// Retryable reports whether a call is worth repeating and the reason label.
func Retryable(err error) (bool, string) {
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusTooManyRequests || ae.Code == "ratelimited":
			return true, "429"
		case ae.StatusCode >= 500 || serverSideCodes[ae.Code]:
			return true, "5xx"
		default:
			return false, "4xx"
		}
	}
	return true, "network"
}

type client struct {
	cfg     Config
	http    *http.Client
	limiter *Limiter
	log     *slog.Logger
}

func New(cfg Config, limiter *Limiter, logger *slog.Logger) Client {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	return &client{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout}, limiter: limiter, log: logger}
}

func (c *client) PostMessage(ctx context.Context, msg Message) (string, error) {
	if c.cfg.DryRun {
		c.log.Info("dry run: skip slack send", "channel", msg.Channel, "text_len", len(msg.Text))
		return "", nil
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal chat.postMessage: %w", err)
	}
	var wait func(context.Context) error
	if c.limiter != nil {
		wait = func(ctx context.Context) error {
			waited, err := c.limiter.Wait(ctx, msg.Channel)
			if err != nil {
				return fmt.Errorf("rate limiter wait: %w", err)
			}
			if waited > 50*time.Millisecond {
				metrics.SlackRateLimited.WithLabelValues(msg.Channel).Inc()
			}
			return nil
		}
	}
	raw, err := c.callWithRetry(ctx, "chat.postMessage", body, wait)
	if err != nil {
		return "", err
	}
	var resp struct {
		TS string `json:"ts"`
	}
	_ = json.Unmarshal(raw, &resp)
	return resp.TS, nil
}

func (c *client) AuthTest(ctx context.Context) error {
	_, err := c.callWithRetry(ctx, "auth.test", []byte("{}"), nil)
	return err
}

// retrySafetyMargin mirrors the Telegram client: keep enough of the request
// budget after a backoff to talk to Slack and still ACK the caller.
const retrySafetyMargin = 500 * time.Millisecond

func (c *client) callWithRetry(ctx context.Context, method string, body []byte, wait func(context.Context) error) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if wait != nil {
			if err := wait(ctx); err != nil {
				return nil, err
			}
		}
		raw, err := c.callOnce(ctx, method, body)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		retryable, reason := Retryable(err)
		if !retryable || attempt == c.cfg.MaxAttempts-1 {
			if !retryable {
				return nil, err
			}
			break
		}

		backoff := c.cfg.InitialBackoff << attempt
		if backoff <= 0 || backoff > c.cfg.MaxBackoff {
			backoff = c.cfg.MaxBackoff
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.RetryAfter > 0 {
			backoff = min(ae.RetryAfter, c.cfg.MaxBackoff)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < backoff+retrySafetyMargin {
			c.log.Warn("slack retry aborted: insufficient deadline budget",
				"method", method, "attempt", attempt+1, "reason", reason, "err", err.Error())
			metrics.SlackRetries.WithLabelValues("deadline_skip").Inc()
			return nil, err
		}
		metrics.SlackRetries.WithLabelValues(reason).Inc()
		c.log.Warn("slack retry", "method", method, "attempt", attempt+1, "reason", reason,
			"backoff_ms", backoff.Milliseconds(), "err", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

func (c *client) callOnce(ctx context.Context, method string, body []byte) ([]byte, error) {
	endpoint, err := url.JoinPath(c.cfg.APIURL, method)
	if err != nil {
		return nil, fmt.Errorf("slack endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, c.scrub(fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	start := time.Now()
	resp, err := c.http.Do(req)
	metrics.SlackAPIDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, c.scrub(fmt.Errorf("http call: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ae := &APIError{StatusCode: resp.StatusCode}
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				ae.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return nil, ae
	}
	var ok struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	if !ok.OK {
		return nil, &APIError{StatusCode: resp.StatusCode, Code: ok.Error}
	}
	return raw, nil
}

// scrub keeps the bot token out of error messages. It travels in a header,
// not the URL, so transport errors should not contain it — this is a guard
// against that ever changing.
func (c *client) scrub(err error) error {
	if err == nil || c.cfg.Token == "" || !strings.Contains(err.Error(), c.cfg.Token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.cfg.Token, "<redacted>"))
}

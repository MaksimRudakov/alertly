package telegram

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

type Client interface {
	// SendMessage sends text to chatID; on success returns the Telegram message_id
	// (0 when DryRun is on, since no network call is made).
	SendMessage(ctx context.Context, chatID int64, threadID *int, text string, opts *SendOptions) (int64, error)
	GetMe(ctx context.Context) error
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error)
	AnswerCallbackQuery(ctx context.Context, callbackID, text string, showAlert bool) error
	EditMessageText(ctx context.Context, chatID int64, messageID int64, text string, markup *InlineKeyboardMarkup) error
	EditMessageReplyMarkup(ctx context.Context, chatID int64, messageID int64, markup *InlineKeyboardMarkup) error
}

type SendOptions struct {
	ReplyMarkup *InlineKeyboardMarkup
}

type Config struct {
	APIURL         string
	Token          string
	ParseMode      string
	RequestTimeout time.Duration
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	DryRun         bool
	// PollMessages adds "message" to getUpdates allowed_updates so chat-ops
	// commands reach the poller. Off unless updates.commands.enabled.
	PollMessages bool
}

type client struct {
	cfg  Config
	http *http.Client
	// pollHTTP has no client-level timeout: long-poll duration is bounded by the
	// request context in GetUpdates. A dedicated client keeps its keep-alive
	// connection to Telegram warm between polls.
	pollHTTP *http.Client
	limiter  *Limiter
	log      *slog.Logger
}

func New(cfg Config, limiter *Limiter, logger *slog.Logger) Client {
	if cfg.ParseMode == "" {
		cfg.ParseMode = "HTML"
	}
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
	return &client{
		cfg:      cfg,
		http:     &http.Client{Timeout: cfg.RequestTimeout},
		pollHTTP: &http.Client{},
		limiter:  limiter,
		log:      logger,
	}
}

type sendMessagePayload struct {
	ChatID                int64                 `json:"chat_id"`
	Text                  string                `json:"text"`
	ParseMode             string                `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool                  `json:"disable_web_page_preview"`
	MessageThreadID       *int                  `json:"message_thread_id,omitempty"`
	ReplyMarkup           *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type apiResponse struct {
	OK          bool             `json:"ok"`
	Description string           `json:"description"`
	ErrorCode   int              `json:"error_code"`
	Parameters  *responseParams  `json:"parameters,omitempty"`
	Result      *json.RawMessage `json:"result,omitempty"`
}

type responseParams struct {
	RetryAfter int `json:"retry_after"`
}

func (c *client) SendMessage(ctx context.Context, chatID int64, threadID *int, text string, opts *SendOptions) (int64, error) {
	if c.cfg.DryRun {
		c.log.Info("dry run: skip telegram send",
			"chat_id", chatID,
			"thread_id", ThreadIDValue(threadID),
			"text_len", len(text),
		)
		return 0, nil
	}

	payload := sendMessagePayload{
		ChatID:                chatID,
		Text:                  text,
		ParseMode:             c.cfg.ParseMode,
		DisableWebPagePreview: true,
		MessageThreadID:       threadID,
	}
	if opts != nil {
		payload.ReplyMarkup = opts.ReplyMarkup
	}

	endpoint := c.endpoint("sendMessage")
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal sendMessage payload: %w", err)
	}

	okBody, err := c.callWithRetry(ctx, endpoint, body, c.chatWait(chatID))
	if err != nil {
		return 0, err
	}
	return parseMessageID(okBody), nil
}

func (c *client) GetMe(ctx context.Context) error {
	endpoint := c.endpoint("getMe")
	// Health probe: deliberately not rate limited so a busy send queue cannot
	// delay readiness detection.
	_, err := c.callWithRetry(ctx, endpoint, nil, nil)
	return err
}

// chatWait returns a per-attempt limiter wait for calls addressed to a chat:
// global + per-chat quota, metered when the wait was long enough to matter.
// Waiting inside the retry loop (not once before it) keeps retries from
// bypassing the quota — five backoff attempts are five API calls.
func (c *client) chatWait(chatID int64) func(context.Context) error {
	if c.limiter == nil {
		return nil
	}
	return func(ctx context.Context) error {
		waited, err := c.limiter.Wait(ctx, chatID)
		if err != nil {
			return fmt.Errorf("rate limiter wait: %w", err)
		}
		if waited > 50*time.Millisecond {
			metrics.TelegramRateLimited.WithLabelValues(metrics.ChatLabel(chatID)).Inc()
		}
		return nil
	}
}

// globalWait returns a per-attempt limiter wait consuming only the global
// quota, for calls without a chat (answerCallbackQuery).
func (c *client) globalWait() func(context.Context) error {
	if c.limiter == nil {
		return nil
	}
	return func(ctx context.Context) error {
		if err := c.limiter.WaitGlobal(ctx); err != nil {
			return fmt.Errorf("rate limiter wait: %w", err)
		}
		return nil
	}
}

func parseMessageID(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var ar struct {
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return 0
	}
	return ar.Result.MessageID
}

func (c *client) endpoint(method string) string {
	base, _ := url.JoinPath(c.cfg.APIURL, "bot"+c.cfg.Token, method)
	return base
}

// retrySafetyMargin is the budget we keep before ctx.Deadline so that, after
// the retry sleep, we still have time to talk to Telegram and write a response
// back to the caller (Alertmanager). Without this, alertly may successfully
// deliver a message to Telegram but fail to ACK the webhook, causing the
// caller to retry — and the message to be sent twice.
const retrySafetyMargin = 500 * time.Millisecond

func (c *client) callWithRetry(ctx context.Context, endpoint string, body []byte, wait func(context.Context) error) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if wait != nil {
			if err := wait(ctx); err != nil {
				return nil, err
			}
		}
		okBody, err := c.callOnce(ctx, endpoint, body)
		if err == nil {
			return okBody, nil
		}
		lastErr = err

		retryable, reason := IsRetryable(err)
		if !retryable {
			return nil, err
		}
		if attempt == c.cfg.MaxAttempts-1 {
			break
		}

		wait := backoff(c.cfg.InitialBackoff, c.cfg.MaxBackoff, attempt)
		var ae *APIError
		if reason == "429" {
			var as *APIError
			if errors.As(err, &as) && as.RetryAfter > 0 {
				wait = as.RetryAfter
				if wait > c.cfg.MaxBackoff {
					wait = c.cfg.MaxBackoff
				}
				ae = as
			}
		}

		if dl, ok := ctx.Deadline(); ok {
			remaining := time.Until(dl)
			if remaining < wait+retrySafetyMargin {
				c.log.Warn("telegram retry aborted: insufficient deadline budget",
					"attempt", attempt+1,
					"reason", reason,
					"backoff_ms", wait.Milliseconds(),
					"remaining_ms", remaining.Milliseconds(),
					"err", err.Error(),
				)
				metrics.TelegramRetries.WithLabelValues("deadline_skip").Inc()
				return nil, err
			}
		}

		metrics.TelegramRetries.WithLabelValues(reason).Inc()

		c.log.Warn("telegram retry",
			"attempt", attempt+1,
			"reason", reason,
			"backoff_ms", wait.Milliseconds(),
			"err", err.Error(),
			"retry_after_hdr", ae != nil,
		)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func (c *client) callOnce(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	}
	if err != nil {
		return nil, c.scrub(fmt.Errorf("build request: %w", err))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	metrics.TelegramAPIDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, c.scrub(fmt.Errorf("http call: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var ar apiResponse
		if err := json.Unmarshal(respBody, &ar); err == nil && !ar.OK {
			return nil, c.scrub(&APIError{StatusCode: resp.StatusCode, Body: ar.Description})
		}
		return respBody, nil
	}

	ae := &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	var ar apiResponse
	if err := json.Unmarshal(respBody, &ar); err == nil {
		if ar.Description != "" {
			ae.Body = ar.Description
		}
		if ar.Parameters != nil && ar.Parameters.RetryAfter > 0 {
			ae.RetryAfter = time.Duration(ar.Parameters.RetryAfter) * time.Second
		}
	}
	if ae.RetryAfter == 0 {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				ae.RetryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	return nil, c.scrub(ae)
}

// redactedToken is what a bot token is replaced with in error strings.
const redactedToken = "<redacted>"

// scrubbedError hides the bot token in the message while keeping the original
// error reachable through errors.Is/As.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub removes the bot token from an error message. Telegram authenticates by
// carrying the token in the request path, and net/http puts the full URL into
// *url.Error — so without this every network failure writes the bot credentials
// into the logs (and into the webhook caller's error line).
func (c *client) scrub(err error) error {
	return scrubToken(err, c.cfg.Token)
}

func scrubToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, token) {
		return err
	}
	return &scrubbedError{msg: strings.ReplaceAll(msg, token, redactedToken), err: err}
}

// ThreadIDValue renders an optional topic thread ID for structured logging:
// nil stays nil instead of a pointer address.
func ThreadIDValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

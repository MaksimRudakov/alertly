package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// UpdatesPoller runs a long-poll loop against Telegram getUpdates and
// dispatches callback_query events to a CallbackHandler. It exits cleanly
// on context cancellation.
type UpdatesPoller struct {
	Client      telegram.Client
	Handler     *CallbackHandler
	Logger      *slog.Logger
	PollTimeout time.Duration
	// HandleTimeout bounds processing of a single callback_query; zero means
	// callbackHandleTimeout.
	HandleTimeout time.Duration

	offset int64
}

func (p *UpdatesPoller) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	handleTimeout := p.HandleTimeout
	if handleTimeout <= 0 {
		handleTimeout = callbackHandleTimeout
	}

	p.Logger.Info("telegram updates poller started", "poll_timeout", p.PollTimeout)
	defer p.Logger.Info("telegram updates poller stopped")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		pollCtx, cancel := context.WithTimeout(ctx, p.PollTimeout+30*time.Second)
		updates, err := p.Client.GetUpdates(pollCtx, p.offset, p.PollTimeout)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			reason := "network"
			var ae *telegram.APIError
			if errors.As(err, &ae) {
				reason = "api_" + httpStatusClass(ae.Status())
			}
			metrics.UpdatesPollErrors.WithLabelValues(reason).Inc()
			p.Logger.Warn("getUpdates failed", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			if u.UpdateID >= p.offset {
				p.offset = u.UpdateID + 1
			}
			if u.CallbackQuery == nil {
				continue
			}
			// Process each callback in the foreground; serialisation gives
			// predictable ordering without extra locking. The per-callback
			// timeout keeps one stuck AM/Telegram call from stalling the loop:
			// without it, EditMessageReplyMarkup would retry with backoff for
			// minutes on an undeadlined context.
			hctx, hcancel := context.WithTimeout(ctx, handleTimeout)
			p.Handler.Handle(hctx, u.CallbackQuery)
			hcancel()
		}
	}
}

// callbackHandleTimeout bounds the processing of a single callback_query.
// Telegram expects answerCallbackQuery within ~15s; anything slower already
// shows a spinner to the user, so there is no point retrying past this budget.
const callbackHandleTimeout = 15 * time.Second

func httpStatusClass(code int) string {
	switch {
	case code == 429:
		return "429"
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	default:
		return "other"
	}
}

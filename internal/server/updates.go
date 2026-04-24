package server

import (
	"context"
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

	offset int64
}

func (p *UpdatesPoller) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

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
			if ae, ok := asTelegramAPIError(err); ok {
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
			// Process each callback in the foreground; AM calls are quick and
			// serialisation gives predictable ordering without extra locking.
			p.Handler.Handle(ctx, u.CallbackQuery)
		}
	}
}

func asTelegramAPIError(err error) (*telegram.APIError, bool) {
	if err == nil {
		return nil, false
	}
	type unwrapper interface{ Unwrap() error }
	for e := err; e != nil; {
		if ae, ok := e.(*telegram.APIError); ok {
			return ae, true
		}
		u, ok := e.(unwrapper)
		if !ok {
			return nil, false
		}
		e = u.Unwrap()
	}
	return nil, false
}

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

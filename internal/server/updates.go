package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// UpdatesPoller runs a long-poll loop against Telegram getUpdates and
// dispatches callback_query events to a CallbackHandler. It exits cleanly
// on context cancellation.
type UpdatesPoller struct {
	Client  telegram.Client
	Handler *CallbackHandler
	// Messages handles chat-ops command messages; nil ignores message updates
	// (they are not requested from Telegram unless commands are enabled).
	Messages    *MessageHandler
	Logger      *slog.Logger
	PollTimeout time.Duration
	// HandleTimeout bounds processing of a single callback_query; zero means
	// callbackHandleTimeout.
	HandleTimeout time.Duration

	offset int64

	// conflictSince/lastConflict track a streak of 409 Conflict responses so
	// the expected overlap of a rolling update is not reported as an error.
	conflictSince time.Time
	lastConflict  time.Time
	now           func() time.Time
}

const (
	// conflictPersistAfter separates the rollout overlap (old and new pod poll
	// for a few seconds until the old one exits) from a genuine second
	// poller, which conflicts indefinitely.
	conflictPersistAfter = 2 * time.Minute
	// conflictStreakGap ends a streak: a conflict after this much quiet
	// starts a new one.
	conflictStreakGap = 5 * time.Minute
)

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
			// Telegram allows one getUpdates consumer per bot token; 409 means
			// another process (a second replica, another alertly, a dev run
			// with the prod token) is polling, and button presses are split
			// between the two.
			if ae != nil && ae.Status() == http.StatusConflict {
				reason = "conflict"
				p.reportConflict(err, backoff)
			} else {
				p.Logger.Warn("getUpdates failed", "err", err, "backoff", backoff)
			}
			metrics.UpdatesPollErrors.WithLabelValues(reason).Inc()
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
			switch {
			case u.CallbackQuery != nil:
				// Process each callback in the foreground; serialisation gives
				// predictable ordering without extra locking. The per-callback
				// timeout keeps one stuck AM/Telegram call from stalling the loop:
				// without it, EditMessageReplyMarkup would retry with backoff for
				// minutes on an undeadlined context.
				p.dispatch(ctx, handleTimeout, u.UpdateID, func(hctx context.Context) {
					p.Handler.Handle(hctx, u.CallbackQuery)
				})
			case u.Message != nil && p.Messages != nil:
				p.dispatch(ctx, handleTimeout, u.UpdateID, func(hctx context.Context) {
					p.Messages.Handle(hctx, u.Message)
				})
			}
		}
	}
}

// reportConflict logs a 409 from getUpdates. A short streak is the normal
// rolling-update overlap and stays a warning; one lasting conflictPersistAfter
// means another process keeps polling this bot token.
func (p *UpdatesPoller) reportConflict(err error, backoff time.Duration) {
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	if p.conflictSince.IsZero() || now.Sub(p.lastConflict) > conflictStreakGap {
		p.conflictSince = now
	}
	p.lastConflict = now

	if streak := now.Sub(p.conflictSince); streak >= conflictPersistAfter {
		p.Logger.Error("getUpdates conflict persists: another instance is polling this bot token; silence buttons and commands will misbehave until it stops",
			"err", err, "streak", streak.Truncate(time.Second), "backoff", backoff)
		return
	}
	p.Logger.Warn("getUpdates conflict (expected briefly during a rolling update)",
		"err", err, "backoff", backoff)
}

// dispatch runs one update handler under the per-update timeout and a panic
// guard. Webhook handlers sit behind recoverMiddleware, but the poller runs
// outside the HTTP stack: without this guard a panic in a callback or command
// handler would take down the whole process. The offset has already been
// advanced, so a poisoned update is not re-fetched in a crash loop.
func (p *UpdatesPoller) dispatch(ctx context.Context, timeout time.Duration, updateID int64, fn func(context.Context)) {
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			p.Logger.Error("panic in update handler recovered",
				"update_id", updateID,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn(hctx)
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

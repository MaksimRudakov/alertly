package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

type webhookDeps struct {
	source       source.Source
	renderer     tmpl.Renderer
	tg           telegram.Client
	readiness    ReadinessTracker
	maxBodyBytes int64
	templateName string
	keyboard     KeyboardBuilder
	tracker      ButtonRegistrar
	dedup        *dedup.Cache
}

// ButtonRegistrar records sent alert messages so the callback handler can
// validate and the sweeper can expire them. The handler only needs Register;
// the full ButtonTracker also exposes Valid/Sweep for consumers.
type ButtonRegistrar interface {
	Register(chatID, messageID int64, fingerprint string)
}

// KeyboardBuilder returns an inline keyboard for a given chat + notification,
// or nil if no keyboard should be attached. Always safe to return nil.
type KeyboardBuilder interface {
	Build(target notification.ChatTarget, n notification.Notification, sourceName string) *telegram.SendOptions
}

func (d webhookDeps) keyboardFor(target notification.ChatTarget, n notification.Notification) *telegram.SendOptions {
	if d.keyboard == nil {
		return nil
	}
	return d.keyboard.Build(target, n, d.source.Name())
}

func webhookHandler(d webhookDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := loggerFrom(ctx).With("source", d.source.Name())

		chatsRaw := r.PathValue("chats")
		targets, err := parseChatTargets(chatsRaw)
		if err != nil {
			logger.Warn("invalid chat list", "raw", chatsRaw, "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			metrics.NotificationsReceived.WithLabelValues(d.source.Name(), "400").Inc()
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBodyBytes))
		if err != nil {
			logger.Warn("read body failed", "err", err)
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			metrics.NotificationsReceived.WithLabelValues(d.source.Name(), "413").Inc()
			return
		}

		parseStart := time.Now()
		notes, err := d.source.Parse(body)
		metrics.SourceParseDuration.WithLabelValues(d.source.Name()).Observe(time.Since(parseStart).Seconds())
		if err != nil {
			logger.Warn("parse failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			metrics.NotificationsReceived.WithLabelValues(d.source.Name(), "400").Inc()
			return
		}

		var (
			totalAttempts int
			totalErrors   int
		)
		for _, n := range notes {
			rendered, err := d.renderer.Render(d.templateName, n)
			if err != nil {
				logger.Error("render failed", "fingerprint", n.Fingerprint, "err", err)
				metrics.TemplateRenderErrors.WithLabelValues(d.templateName).Inc()
				totalErrors++
				totalAttempts++
				continue
			}

			parts := telegram.SplitMessage(rendered, telegram.TelegramTextLimit)
			if len(parts) > 1 {
				metrics.MessageSplitTotal.Inc()
			}

			for _, target := range targets {
				dedupKey := dedup.Key(n.Fingerprint, target.ChatID, target.ThreadID, n.Status)
				if d.dedup.Reserve(dedupKey) {
					metrics.DedupSkipped.WithLabelValues(d.source.Name(),
						strconv.FormatInt(target.ChatID, 10), n.Status).Inc()
					logger.Info("dedup: skip duplicate delivery",
						"chat_id", target.ChatID,
						"thread_id", threadIDValue(target.ThreadID),
						"fingerprint", n.Fingerprint,
						"status", n.Status,
					)
					continue
				}

				targetSentAny := false
				targetFailed := false
				for idx, part := range parts {
					totalAttempts++
					var opts *telegram.SendOptions
					// Attach inline keyboard only to the last message part, so buttons
					// are attached once per (notification, chat) pair.
					isLastPart := idx == len(parts)-1
					if isLastPart {
						opts = d.keyboardFor(target, n)
					}
					messageID, err := d.send(ctx, target, part, opts)
					if err != nil {
						totalErrors++
						targetFailed = true
						logger.Error("send failed",
							"chat_id", target.ChatID,
							"thread_id", threadIDValue(target.ThreadID),
							"fingerprint", n.Fingerprint,
							"err", err,
						)
						metrics.NotificationsSent.WithLabelValues(strconv.FormatInt(target.ChatID, 10), "error").Inc()
						continue
					}
					targetSentAny = true
					metrics.NotificationsSent.WithLabelValues(strconv.FormatInt(target.ChatID, 10), "ok").Inc()
					if isLastPart && opts != nil && d.tracker != nil && messageID != 0 {
						d.tracker.Register(target.ChatID, messageID, n.Fingerprint)
					}
				}
				// Roll back the reservation only when nothing was delivered: the
				// caller's retry should be allowed to deliver it for real. If at
				// least one part landed, we keep the reservation so the retry
				// does not duplicate the part(s) already in the chat.
				if !targetSentAny && targetFailed {
					d.dedup.Forget(dedupKey)
				}
			}
		}

		var status int
		switch {
		case totalAttempts == 0:
			status = http.StatusNoContent
		case totalErrors == 0:
			status = http.StatusOK
		case totalErrors < totalAttempts:
			status = http.StatusMultiStatus
		default:
			status = http.StatusInternalServerError
		}

		metrics.NotificationsReceived.WithLabelValues(d.source.Name(), strconv.Itoa(status)).Inc()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"attempts":%d,"errors":%d}`, totalAttempts, totalErrors)
	}
}

func (d webhookDeps) send(ctx context.Context, target notification.ChatTarget, text string, opts *telegram.SendOptions) (int64, error) {
	messageID, err := d.tg.SendMessage(ctx, target.ChatID, target.ThreadID, text, opts)
	if err == nil {
		d.readiness.RecordSendSuccess()
		return messageID, nil
	}
	d.readiness.RecordSendFailure(isServerError(err))
	return 0, err
}

func isServerError(err error) bool {
	var ae *telegram.APIError
	if errors.As(err, &ae) {
		return ae.Status() >= 500
	}
	return false
}

func threadIDValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

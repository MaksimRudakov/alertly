package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

const (
	CallbackActionSilence = "s"
	CallbackActionUndo    = "u"
	callbackFieldSep      = "|"
	// callbackFieldNone fills the unused third field of undo callback data so
	// every callback keeps the same 3-field wire format.
	callbackFieldNone = "-"
)

// CallbackDeps carries dependencies for handling Telegram callback_query events.
type CallbackDeps struct {
	Logger        *slog.Logger
	Telegram      telegram.Client
	AM            alertmanager.Client
	Cache         *alertmanager.LabelCache
	Tracker       *ButtonTracker
	ChatAllowlist []int64
	UserAllowlist []int64
	Durations     map[string]time.Duration // "1h" -> 1h, pre-validated at startup
	// SilenceMatchers limits which labels become matchers (empty = all).
	SilenceMatchers []string
	// UndoTracker holds messages carrying a live ↩️ Undo button. nil disables undo.
	UndoTracker *ButtonTracker
}

// CallbackHandler processes a single callback_query: validates allowlists,
// resolves labels, creates a silence, acks the callback, edits the message.
type CallbackHandler struct {
	deps CallbackDeps
}

func NewCallbackHandler(deps CallbackDeps) *CallbackHandler {
	return &CallbackHandler{deps: deps}
}

// Handle processes one callback_query. Errors from this method are logged
// but never propagated — the long-poll loop must keep running.
func (h *CallbackHandler) Handle(ctx context.Context, cq *telegram.CallbackQuery) {
	if cq == nil {
		return
	}
	logger := h.deps.Logger.With(
		"callback_id", cq.ID,
		"user_id", cq.From.ID,
		"username", cq.From.Username,
	)
	if cq.Message != nil {
		logger = logger.With("chat_id", cq.Message.Chat.ID, "message_id", cq.Message.MessageID)
	}

	action, fingerprint, durationKey, err := ParseCallbackData(cq.Data)
	if err != nil {
		metrics.CallbacksReceived.WithLabelValues("unknown", "invalid").Inc()
		logger.Warn("callback: invalid data", "data", cq.Data, "err", err)
		h.answer(ctx, cq.ID, "⚠️ Invalid callback.", true)
		return
	}
	logger = logger.With("action", action, "fingerprint", fingerprint, "duration", durationKey)

	if action != CallbackActionSilence && action != CallbackActionUndo {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: unknown action")
		h.answer(ctx, cq.ID, "⚠️ Unknown action.", true)
		return
	}

	if cq.Message == nil {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: missing message")
		h.answer(ctx, cq.ID, "⚠️ Missing message context.", true)
		return
	}

	chatID := cq.Message.Chat.ID
	if !int64InSet(chatID, h.deps.ChatAllowlist) {
		metrics.CallbacksReceived.WithLabelValues(action, "auth_failed").Inc()
		logger.Warn("callback: chat not in allowlist")
		h.answer(ctx, cq.ID, "⛔ This chat cannot silence alerts.", true)
		return
	}
	if len(h.deps.UserAllowlist) > 0 && !int64InSet(cq.From.ID, h.deps.UserAllowlist) {
		metrics.CallbacksReceived.WithLabelValues(action, "auth_failed").Inc()
		logger.Warn("callback: user not in allowlist")
		h.answer(ctx, cq.ID, "⛔ You are not authorized to silence alerts.", true)
		return
	}

	if action == CallbackActionUndo {
		// For undo the second field carries the silence ID, not a fingerprint.
		h.handleUndo(ctx, cq, fingerprint, logger)
		return
	}

	// Window check: strict — if the message is not tracked or has expired,
	// reject the click and strip the keyboard so it is clear nothing will happen.
	if !h.deps.Tracker.Valid(chatID, cq.Message.MessageID) {
		metrics.CallbacksReceived.WithLabelValues(action, "expired").Inc()
		logger.Warn("callback: silence window expired or unknown message")
		h.stripKeyboard(ctx, cq)
		h.answer(ctx, cq.ID, "⏰ Silence window expired for this alert.", true)
		return
	}

	duration, ok := h.deps.Durations[durationKey]
	if !ok {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: duration not configured", "duration", durationKey)
		h.answer(ctx, cq.ID, "⚠️ Unsupported silence duration.", true)
		return
	}

	labels, err := h.resolveLabels(ctx, fingerprint)
	if err != nil {
		if errors.Is(err, alertmanager.ErrAlertNotFound) {
			metrics.CallbacksReceived.WithLabelValues(action, "not_found").Inc()
			logger.Warn("callback: alert not found")
			h.answer(ctx, cq.ID, "⚠️ Alert no longer active and not in cache.", true)
			return
		}
		metrics.CallbacksReceived.WithLabelValues(action, "am_error").Inc()
		logger.Error("callback: resolve labels failed", "err", err)
		h.answer(ctx, cq.ID, "⚠️ Failed to query Alertmanager.", true)
		return
	}

	matchers := alertmanager.MatchersFromLabels(labels, h.deps.SilenceMatchers)
	if len(matchers) == 0 {
		// None of the configured silence_matchers labels exist on this alert; a
		// zero-matcher silence would match everything — refuse.
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: no matchers after silence_matchers filter",
			"silence_matchers", h.deps.SilenceMatchers)
		h.answer(ctx, cq.ID, "⚠️ Alert has none of the configured silence labels.", true)
		return
	}

	now := time.Now().UTC()
	silenceID, err := h.deps.AM.CreateSilence(ctx, alertmanager.SilenceRequest{
		Matchers:  matchers,
		StartsAt:  now,
		EndsAt:    now.Add(duration),
		CreatedBy: silenceCreatedBy(cq.From),
		Comment:   fmt.Sprintf("silenced via alertly by %s from chat %d", silenceCreatedBy(cq.From), chatID),
	})
	if err != nil {
		metrics.CallbacksReceived.WithLabelValues(action, "am_error").Inc()
		metrics.SilencesCreated.WithLabelValues("error").Inc()
		logger.Error("callback: create silence failed", "err", err)
		h.answer(ctx, cq.ID, "⚠️ Alertmanager rejected the silence.", true)
		return
	}

	metrics.CallbacksReceived.WithLabelValues(action, "ok").Inc()
	metrics.SilencesCreated.WithLabelValues("ok").Inc()
	logger.Info("silence created", "silence_id", silenceID, "until", now.Add(duration))

	// Silence buttons must go away so nobody silences twice; when undo is
	// enabled they are replaced with a short-lived ↩️ Undo button instead.
	h.deps.Tracker.Consume(chatID, cq.Message.MessageID)
	if h.deps.UndoTracker != nil && len(BuildCallbackData(CallbackActionUndo, silenceID, callbackFieldNone)) <= maxCallbackDataBytes {
		h.deps.UndoTracker.Register(chatID, cq.Message.MessageID, silenceID)
		if err := h.deps.Telegram.EditMessageReplyMarkup(ctx, chatID, cq.Message.MessageID, undoKeyboard(silenceID)); err != nil {
			h.deps.Logger.Warn("callback: attach undo keyboard failed", "err", err)
		}
	} else {
		h.stripKeyboard(ctx, cq)
	}
	until := now.Add(duration).Format("15:04 MST")
	h.answer(ctx, cq.ID, fmt.Sprintf("🔇 Silenced %s until %s (id: %s)", durationKey, until, silenceID), false)
}

// handleUndo deletes the silence referenced by the undo button. The undo
// window is enforced by UndoTracker (strict: restart or expiry rejects).
func (h *CallbackHandler) handleUndo(ctx context.Context, cq *telegram.CallbackQuery, silenceID string, logger *slog.Logger) {
	chatID := cq.Message.Chat.ID
	if h.deps.UndoTracker == nil || !h.deps.UndoTracker.Valid(chatID, cq.Message.MessageID) {
		metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, "expired").Inc()
		logger.Warn("callback: undo window expired or unknown message")
		h.stripKeyboard(ctx, cq)
		h.answer(ctx, cq.ID, "⏰ Undo window expired; remove the silence in Alertmanager if needed.", true)
		return
	}

	if err := h.deps.AM.DeleteSilence(ctx, silenceID); err != nil {
		metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, "am_error").Inc()
		metrics.SilencesDeleted.WithLabelValues("error").Inc()
		logger.Error("callback: delete silence failed", "silence_id", silenceID, "err", err)
		h.answer(ctx, cq.ID, "⚠️ Failed to remove the silence in Alertmanager.", true)
		return
	}

	metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, "ok").Inc()
	metrics.SilencesDeleted.WithLabelValues("ok").Inc()
	logger.Info("silence removed via undo", "silence_id", silenceID)

	h.deps.UndoTracker.Consume(chatID, cq.Message.MessageID)
	h.stripKeyboard(ctx, cq)
	h.answer(ctx, cq.ID, "🔊 Silence removed — alert will notify again.", false)
}

func undoKeyboard(silenceID string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegram.InlineKeyboardButton{{{
			Text:         "↩️ Undo silence",
			CallbackData: BuildCallbackData(CallbackActionUndo, silenceID, callbackFieldNone),
		}}},
	}
}

func (h *CallbackHandler) resolveLabels(ctx context.Context, fingerprint string) (map[string]string, error) {
	labels, err := h.deps.AM.GetAlertLabels(ctx, fingerprint)
	if err == nil {
		return labels, nil
	}
	if errors.Is(err, alertmanager.ErrAlertNotFound) {
		if cached, ok := h.deps.Cache.Get(fingerprint); ok {
			metrics.LabelCacheLookups.WithLabelValues("hit").Inc()
			return cached, nil
		}
		metrics.LabelCacheLookups.WithLabelValues("miss").Inc()
		return nil, alertmanager.ErrAlertNotFound
	}
	return nil, err
}

func (h *CallbackHandler) stripKeyboard(ctx context.Context, cq *telegram.CallbackQuery) {
	if cq.Message == nil {
		return
	}
	if err := h.deps.Telegram.EditMessageReplyMarkup(ctx, cq.Message.Chat.ID, cq.Message.MessageID, nil); err != nil {
		h.deps.Logger.Warn("callback: edit reply markup failed", "err", err)
	}
}

func (h *CallbackHandler) answer(ctx context.Context, id, text string, showAlert bool) {
	// Answer must be sent within ~15s or Telegram shows "loading…". Detach from
	// the parent so the user still gets feedback when the per-callback handle
	// budget was spent inside an AM call, but keep our own short timeout.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := h.deps.Telegram.AnswerCallbackQuery(cctx, id, text, showAlert); err != nil {
		h.deps.Logger.Warn("answerCallbackQuery failed", "err", err)
	}
}

// ParseCallbackData parses "s|<fp>|<dur>" into its parts.
func ParseCallbackData(data string) (action, fingerprint, durationKey string, err error) {
	parts := strings.Split(data, callbackFieldSep)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("expected 3 fields, got %d", len(parts))
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", errors.New("empty field in callback data")
	}
	return parts[0], parts[1], parts[2], nil
}

// BuildCallbackData assembles "s|<fp>|<dur>". Caller is responsible for keeping
// the result <=64 bytes (Telegram limit).
func BuildCallbackData(action, fingerprint, durationKey string) string {
	return action + callbackFieldSep + fingerprint + callbackFieldSep + durationKey
}

func int64InSet(v int64, set []int64) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func silenceCreatedBy(u telegram.User) string {
	if u.Username != "" {
		return "telegram:@" + u.Username
	}
	return fmt.Sprintf("telegram:%d", u.ID)
}

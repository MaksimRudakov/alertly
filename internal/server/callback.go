package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

const (
	CallbackActionSilence = "s"
	CallbackActionUndo    = "u"
	callbackFieldSep      = "|"
	// callbackFieldNone fills the unused duration field of undo callback data
	// so silence and undo share one wire format.
	callbackFieldNone = "-"
)

// Callback data wire format: `action|value|duration` for the default cluster
// (unchanged from single-cluster alertly, so buttons already in chats keep
// working) and `action|alias|value|duration` for named clusters. value is the
// alert fingerprint for silence, the silence ID for undo.
type callbackPayload struct {
	Action string
	// Cluster is the cluster alias; "" = default cluster (3-field format).
	Cluster  string
	Value    string
	Duration string
}

// CallbackDeps carries dependencies for handling Telegram callback_query events.
type CallbackDeps struct {
	Logger   *slog.Logger
	Telegram telegram.Client
	// Clusters by alias. nil = single-cluster mode: one default cluster whose
	// Alertmanager is AM.
	Clusters      map[string]*Cluster
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
	if deps.Clusters == nil {
		deps.Clusters = map[string]*Cluster{
			config.DefaultCluster: {Name: config.DefaultCluster, Alias: config.DefaultCluster, Implicit: true, AM: deps.AM},
		}
	}
	return &CallbackHandler{deps: deps}
}

// clusterByAlias resolves the alias carried in callback data ("" = default).
func (h *CallbackHandler) clusterByAlias(alias string) *Cluster {
	if alias == "" {
		alias = config.DefaultCluster
	}
	return h.deps.Clusters[alias]
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

	payload, err := ParseCallbackData(cq.Data)
	action, fingerprint, durationKey := payload.Action, payload.Value, payload.Duration
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

	cluster := h.clusterByAlias(payload.Cluster)
	if cluster == nil || cluster.AM == nil {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: unknown cluster or cluster without alertmanager", "cluster_alias", payload.Cluster)
		h.answer(ctx, cq.ID, "⚠️ Unknown cluster for this alert.", true)
		return
	}
	logger = logger.With("cluster", cluster.Name)

	if action == CallbackActionUndo {
		// For undo the value field carries the silence ID, not a fingerprint.
		h.handleUndo(ctx, cq, cluster, fingerprint, logger)
		return
	}

	// Window check: strict — if the message is not tracked or has expired,
	// reject the click and strip the keyboard so it is clear nothing will happen.
	trackedCluster, tracked, ok := h.deps.Tracker.LookupEntry(chatID, cq.Message.MessageID)
	if !ok {
		metrics.CallbacksReceived.WithLabelValues(action, "expired").Inc()
		logger.Warn("callback: silence window expired or unknown message")
		h.stripKeyboard(ctx, cq)
		h.answer(ctx, cq.ID, "⏰ Silence window expired for this alert.", true)
		return
	}
	// The button must carry the cluster and fingerprint alertly attached to
	// this very message; anything else is a forged or stale payload.
	if tracked != fingerprint || trackedCluster != cluster.Name {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: button does not match the tracked message", "tracked", tracked, "tracked_cluster", trackedCluster)
		h.answer(ctx, cq.ID, "⚠️ Button does not match this alert.", true)
		return
	}

	duration, ok := h.deps.Durations[durationKey]
	if !ok {
		metrics.CallbacksReceived.WithLabelValues(action, "invalid").Inc()
		logger.Warn("callback: duration not configured", "duration", durationKey)
		h.answer(ctx, cq.ID, "⚠️ Unsupported silence duration.", true)
		return
	}

	labels, err := h.resolveLabels(ctx, cluster, fingerprint)
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
	comment := fmt.Sprintf("silenced via alertly by %s from chat %d", silenceCreatedBy(cq.From), chatID)
	if !cluster.Implicit {
		comment += " (cluster " + cluster.Name + ")"
	}
	silenceID, err := cluster.AM.CreateSilence(ctx, alertmanager.SilenceRequest{
		Matchers:  matchers,
		StartsAt:  now,
		EndsAt:    now.Add(duration),
		CreatedBy: silenceCreatedBy(cq.From),
		Comment:   comment,
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
	if h.deps.UndoTracker != nil && len(buildCallbackData(cluster, CallbackActionUndo, silenceID, callbackFieldNone)) <= maxCallbackDataBytes {
		h.deps.UndoTracker.RegisterFor(chatID, cq.Message.MessageID, cluster.Name, silenceID)
		if err := h.deps.Telegram.EditMessageReplyMarkup(ctx, chatID, cq.Message.MessageID, undoKeyboard(cluster, silenceID)); err != nil {
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
func (h *CallbackHandler) handleUndo(ctx context.Context, cq *telegram.CallbackQuery, cluster *Cluster, silenceID string, logger *slog.Logger) {
	chatID := cq.Message.Chat.ID
	trackedCluster, tracked, ok := h.deps.UndoTracker.LookupEntry(chatID, cq.Message.MessageID)
	if !ok {
		metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, "expired").Inc()
		logger.Warn("callback: undo window expired or unknown message")
		h.stripKeyboard(ctx, cq)
		h.answer(ctx, cq.ID, "⏰ Undo window expired; remove the silence in Alertmanager if needed.", true)
		return
	}
	if tracked != silenceID || trackedCluster != cluster.Name {
		metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, "invalid").Inc()
		logger.Warn("callback: silence id does not match the tracked message", "tracked", tracked)
		h.answer(ctx, cq.ID, "⚠️ Button does not match this silence.", true)
		return
	}

	if err := cluster.AM.DeleteSilence(ctx, silenceID); err != nil {
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

func undoKeyboard(cluster *Cluster, silenceID string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegram.InlineKeyboardButton{{{
			Text:         "↩️ Undo silence",
			CallbackData: buildCallbackData(cluster, CallbackActionUndo, silenceID, callbackFieldNone),
		}}},
	}
}

// resolveLabels prefers the live alert from AM and falls back to the labels
// cached when the notification was sent — on "not found" and on any other AM
// failure alike, so an overloaded or briefly unreachable AM does not break the
// button. The cached alertname narrows the AM query server-side.
func (h *CallbackHandler) resolveLabels(ctx context.Context, cluster *Cluster, fingerprint string) (map[string]string, error) {
	cached, cachedOK := h.deps.Cache.Get(labelCacheKey(cluster.Name, fingerprint))
	labels, err := cluster.AM.GetAlertLabels(ctx, fingerprint, cached["alertname"])
	if err == nil {
		return labels, nil
	}
	if cachedOK {
		metrics.LabelCacheLookups.WithLabelValues("hit").Inc()
		if !errors.Is(err, alertmanager.ErrAlertNotFound) {
			h.deps.Logger.Warn("callback: alertmanager lookup failed; using cached labels", "fingerprint", fingerprint, "err", err)
		}
		return cached, nil
	}
	metrics.LabelCacheLookups.WithLabelValues("miss").Inc()
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

// ParseCallbackData parses the 3-field (default cluster) or 4-field (named
// cluster) callback data format.
func ParseCallbackData(data string) (callbackPayload, error) {
	parts := strings.Split(data, callbackFieldSep)
	var p callbackPayload
	switch len(parts) {
	case 3:
		p = callbackPayload{Action: parts[0], Value: parts[1], Duration: parts[2]}
	case 4:
		p = callbackPayload{Action: parts[0], Cluster: parts[1], Value: parts[2], Duration: parts[3]}
		if p.Cluster == "" {
			return callbackPayload{}, errors.New("empty cluster alias in callback data")
		}
	default:
		return callbackPayload{}, fmt.Errorf("expected 3 or 4 fields, got %d", len(parts))
	}
	if p.Action == "" || p.Value == "" || p.Duration == "" {
		return callbackPayload{}, errors.New("empty field in callback data")
	}
	return p, nil
}

// BuildCallbackData assembles the default-cluster "action|value|duration".
// Caller is responsible for keeping the result <=64 bytes (Telegram limit).
func BuildCallbackData(action, value, durationKey string) string {
	return action + callbackFieldSep + value + callbackFieldSep + durationKey
}

// buildCallbackData picks the wire format for the cluster: named clusters
// carry their alias, the default cluster keeps the 3-field format.
func buildCallbackData(cluster *Cluster, action, value, durationKey string) string {
	if cluster == nil || cluster.Name == config.DefaultCluster {
		return BuildCallbackData(action, value, durationKey)
	}
	return action + callbackFieldSep + cluster.Alias + callbackFieldSep + value + callbackFieldSep + durationKey
}

func buildSilenceData(cluster *Cluster, fingerprint, durationKey string) string {
	return buildCallbackData(cluster, CallbackActionSilence, fingerprint, durationKey)
}

// labelCacheKey namespaces cached labels per cluster (Alertmanager
// fingerprints are label hashes and collide across clusters). The default
// cluster keeps the bare fingerprint.
func labelCacheKey(cluster, fingerprint string) string {
	if cluster == config.DefaultCluster || cluster == "" {
		return fingerprint
	}
	return cluster + "|" + fingerprint
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

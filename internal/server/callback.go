package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/sink"
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

// Callback data wire format (Telegram callback_data, Slack button value):
// `action|value|duration` for the default cluster (unchanged from
// single-cluster alertly, so buttons already in chats keep working) and
// `action|alias|value|duration` for named clusters. value is the alert
// fingerprint for silence, the silence ID for undo.
type callbackPayload struct {
	Action string
	// Cluster is the cluster alias; "" = default cluster (3-field format).
	Cluster  string
	Value    string
	Duration string
}

// Actor is the user who pressed a button or sent a command.
type Actor struct {
	ID   string
	Name string // username without @, may be empty
}

// Interaction is a button press in any messenger.
type Interaction struct {
	Sink    string
	Chat    string // Telegram chat ID / Slack channel ID
	Thread  string // Slack thread_ts of the message, for ephemeral replies
	User    Actor
	Message sink.MessageRef
	Data    string
	// Original is the message as the messenger reports it, used to remove
	// buttons when the tracker no longer knows the message (Slack needs the
	// whole body for chat.update; unused for Telegram).
	Original sink.Part
}

// Feedback tells the pressing user what happened (Telegram
// answerCallbackQuery, Slack ephemeral message). alert asks for a modal.
type Feedback func(ctx context.Context, text string, alert bool)

// AccessPolicy limits who may press buttons or run commands in one sink.
// Empty Chats = nobody; empty Users = anyone in an allowed chat.
type AccessPolicy struct {
	Chats []string
	Users []string
}

// CallbackDeps carries dependencies for handling button presses.
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
	// Access per sink; nil = Telegram only, from ChatAllowlist/UserAllowlist.
	Access    map[string]AccessPolicy
	Durations map[string]time.Duration // "1h" -> 1h, pre-validated at startup
	// SilenceMatchers limits which labels become matchers (empty = all).
	SilenceMatchers []string
	// UndoTracker holds messages carrying a live ↩️ Undo button. nil disables undo.
	UndoTracker *ButtonTracker
	// Sinks change buttons on delivered messages; a missing sink falls back
	// to editing through Telegram directly.
	Sinks map[string]sink.Sink
}

// CallbackHandler processes a single button press: validates allowlists,
// resolves labels, creates a silence, gives feedback, updates the buttons.
type CallbackHandler struct {
	deps CallbackDeps
}

func NewCallbackHandler(deps CallbackDeps) *CallbackHandler {
	if deps.Clusters == nil {
		deps.Clusters = map[string]*Cluster{
			config.DefaultCluster: {Name: config.DefaultCluster, Alias: config.DefaultCluster, Implicit: true, AM: deps.AM},
		}
	}
	if deps.Access == nil {
		deps.Access = map[string]AccessPolicy{sink.Telegram: {
			Chats: int64Strings(deps.ChatAllowlist),
			Users: int64Strings(deps.UserAllowlist),
		}}
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

// Handle adapts a Telegram callback_query. Errors are logged, never
// propagated — the long-poll loop must keep running.
func (h *CallbackHandler) Handle(ctx context.Context, cq *telegram.CallbackQuery) {
	if cq == nil {
		return
	}
	in := Interaction{
		Sink: sink.Telegram,
		User: Actor{ID: strconv.FormatInt(cq.From.ID, 10), Name: cq.From.Username},
		Data: cq.Data,
	}
	if cq.Message != nil {
		in.Chat = strconv.FormatInt(cq.Message.Chat.ID, 10)
		in.Message = telegramRef(cq.Message.Chat.ID, cq.Message.MessageID)
	}
	h.HandleInteraction(ctx, in, func(ctx context.Context, text string, alert bool) {
		h.answerTelegram(ctx, cq.ID, text, alert)
	})
}

// HandleInteraction processes one button press from any messenger.
func (h *CallbackHandler) HandleInteraction(ctx context.Context, in Interaction, feedback Feedback) {
	logger := h.deps.Logger.With("sink", in.Sink, "user_id", in.User.ID, "username", in.User.Name)
	if in.Message.ID != "" {
		logger = logger.With("chat", in.Chat, "message", in.Message.ID)
	}
	count := func(action, status string) {
		metrics.CallbacksReceived.WithLabelValues(action, status, in.Sink).Inc()
	}

	payload, err := ParseCallbackData(in.Data)
	action, value, durationKey := payload.Action, payload.Value, payload.Duration
	if err != nil {
		count("unknown", "invalid")
		logger.Warn("callback: invalid data", "data", in.Data, "err", err)
		feedback(ctx, "⚠️ Invalid callback.", true)
		return
	}
	logger = logger.With("action", action, "fingerprint", value, "duration", durationKey)

	if action != CallbackActionSilence && action != CallbackActionUndo {
		count(action, "invalid")
		logger.Warn("callback: unknown action")
		feedback(ctx, "⚠️ Unknown action.", true)
		return
	}

	if in.Message.ID == "" {
		count(action, "invalid")
		logger.Warn("callback: missing message")
		feedback(ctx, "⚠️ Missing message context.", true)
		return
	}

	policy := h.deps.Access[in.Sink]
	if !stringInSet(in.Chat, policy.Chats) {
		count(action, "auth_failed")
		logger.Warn("callback: chat not in allowlist")
		feedback(ctx, "⛔ This chat cannot silence alerts.", true)
		return
	}
	if len(policy.Users) > 0 && !stringInSet(in.User.ID, policy.Users) {
		count(action, "auth_failed")
		logger.Warn("callback: user not in allowlist")
		feedback(ctx, "⛔ You are not authorized to silence alerts.", true)
		return
	}

	cluster := h.clusterByAlias(payload.Cluster)
	if cluster == nil || cluster.AM == nil {
		count(action, "invalid")
		logger.Warn("callback: unknown cluster or cluster without alertmanager", "cluster_alias", payload.Cluster)
		feedback(ctx, "⚠️ Unknown cluster for this alert.", true)
		return
	}
	logger = logger.With("cluster", cluster.Name)

	if action == CallbackActionUndo {
		// For undo the value field carries the silence ID, not a fingerprint.
		h.handleUndo(ctx, in, feedback, cluster, value, logger)
		return
	}

	// Window check: strict — if the message is not tracked or has expired,
	// reject the click and remove the buttons so it is clear nothing happens.
	entry, ok := h.deps.Tracker.Entry(in.Message)
	if !ok {
		count(action, "expired")
		logger.Warn("callback: silence window expired or unknown message")
		h.setActions(ctx, in, in.Original, nil)
		feedback(ctx, "⏰ Silence window expired for this alert.", true)
		return
	}
	// The button must carry the cluster and fingerprint alertly attached to
	// this very message; anything else is a forged or stale payload.
	if entry.Value != value || entry.Cluster != cluster.Name {
		count(action, "invalid")
		logger.Warn("callback: button does not match the tracked message", "tracked", entry.Value, "tracked_cluster", entry.Cluster)
		feedback(ctx, "⚠️ Button does not match this alert.", true)
		return
	}

	duration, ok := h.deps.Durations[durationKey]
	if !ok {
		count(action, "invalid")
		logger.Warn("callback: duration not configured", "duration", durationKey)
		feedback(ctx, "⚠️ Unsupported silence duration.", true)
		return
	}

	labels, err := h.resolveLabels(ctx, cluster, value)
	if err != nil {
		if errors.Is(err, alertmanager.ErrAlertNotFound) {
			count(action, "not_found")
			logger.Warn("callback: alert not found")
			feedback(ctx, "⚠️ Alert no longer active and not in cache.", true)
			return
		}
		count(action, "am_error")
		logger.Error("callback: resolve labels failed", "err", err)
		feedback(ctx, "⚠️ Failed to query Alertmanager.", true)
		return
	}

	matchers := alertmanager.MatchersFromLabels(labels, h.deps.SilenceMatchers)
	if len(matchers) == 0 {
		// None of the configured silence_matchers labels exist on this alert; a
		// zero-matcher silence would match everything — refuse.
		count(action, "invalid")
		logger.Warn("callback: no matchers after silence_matchers filter",
			"silence_matchers", h.deps.SilenceMatchers)
		feedback(ctx, "⚠️ Alert has none of the configured silence labels.", true)
		return
	}

	now := time.Now().UTC()
	by := createdBy(in.Sink, in.User)
	comment := fmt.Sprintf("silenced via alertly by %s from chat %s", by, in.Chat)
	if !cluster.Implicit {
		comment += " (cluster " + cluster.Name + ")"
	}
	silenceID, err := cluster.AM.CreateSilence(ctx, alertmanager.SilenceRequest{
		Matchers:  matchers,
		StartsAt:  now,
		EndsAt:    now.Add(duration),
		CreatedBy: by,
		Comment:   comment,
	})
	if err != nil {
		count(action, "am_error")
		metrics.SilencesCreated.WithLabelValues("error").Inc()
		logger.Error("callback: create silence failed", "err", err)
		feedback(ctx, "⚠️ Alertmanager rejected the silence.", true)
		return
	}

	count(action, "ok")
	metrics.SilencesCreated.WithLabelValues("ok").Inc()
	logger.Info("silence created", "silence_id", silenceID, "until", now.Add(duration))

	// Silence buttons must go away so nobody silences twice; when undo is
	// enabled they are replaced with a short-lived ↩️ Undo button instead.
	h.deps.Tracker.ConsumeRef(in.Message)
	part := entry.Part
	if part.Text == "" && part.Payload == nil {
		part = in.Original
	}
	if h.deps.UndoTracker != nil && len(buildCallbackData(cluster, CallbackActionUndo, silenceID, callbackFieldNone)) <= maxCallbackDataBytes {
		h.deps.UndoTracker.RegisterMessage(in.Message, cluster.Name, silenceID, part)
		if err := h.setActionsErr(ctx, in, part, undoActions(cluster, silenceID)); err != nil {
			logger.Warn("callback: attach undo button failed", "err", err)
		}
	} else {
		h.setActions(ctx, in, part, nil)
	}
	until := now.Add(duration).Format("15:04 MST")
	feedback(ctx, fmt.Sprintf("🔇 Silenced %s until %s (id: %s)", durationKey, until, silenceID), false)
}

// handleUndo deletes the silence referenced by the undo button. The undo
// window is enforced by UndoTracker (strict: restart or expiry rejects).
func (h *CallbackHandler) handleUndo(ctx context.Context, in Interaction, feedback Feedback, cluster *Cluster, silenceID string, logger *slog.Logger) {
	count := func(status string) {
		metrics.CallbacksReceived.WithLabelValues(CallbackActionUndo, status, in.Sink).Inc()
	}
	entry, ok := h.deps.UndoTracker.Entry(in.Message)
	if !ok {
		count("expired")
		logger.Warn("callback: undo window expired or unknown message")
		h.setActions(ctx, in, in.Original, nil)
		feedback(ctx, "⏰ Undo window expired; remove the silence in Alertmanager if needed.", true)
		return
	}
	if entry.Value != silenceID || entry.Cluster != cluster.Name {
		count("invalid")
		logger.Warn("callback: silence id does not match the tracked message", "tracked", entry.Value, "tracked_cluster", entry.Cluster)
		feedback(ctx, "⚠️ Button does not match this silence.", true)
		return
	}

	if err := cluster.AM.DeleteSilence(ctx, silenceID); err != nil {
		count("am_error")
		metrics.SilencesDeleted.WithLabelValues("error").Inc()
		logger.Error("callback: delete silence failed", "silence_id", silenceID, "err", err)
		feedback(ctx, "⚠️ Failed to remove the silence in Alertmanager.", true)
		return
	}

	count("ok")
	metrics.SilencesDeleted.WithLabelValues("ok").Inc()
	logger.Info("silence removed via undo", "silence_id", silenceID)

	h.deps.UndoTracker.ConsumeRef(in.Message)
	part := entry.Part
	if part.Text == "" && part.Payload == nil {
		part = in.Original
	}
	h.setActions(ctx, in, part, nil)
	feedback(ctx, "🔊 Silence removed — alert will notify again.", false)
}

func undoActions(cluster *Cluster, silenceID string) *sink.Actions {
	return &sink.Actions{Rows: [][]sink.Button{{{
		Text: "↩️ Undo silence",
		Data: buildCallbackData(cluster, CallbackActionUndo, silenceID, callbackFieldNone),
	}}}}
}

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

// setActions replaces (nil: removes) the buttons of the pressed message,
// logging failures.
func (h *CallbackHandler) setActions(ctx context.Context, in Interaction, part sink.Part, actions *sink.Actions) {
	if err := h.setActionsErr(ctx, in, part, actions); err != nil {
		h.deps.Logger.Warn("callback: update buttons failed", "sink", in.Sink, "err", err)
	}
}

func (h *CallbackHandler) setActionsErr(ctx context.Context, in Interaction, part sink.Part, actions *sink.Actions) error {
	if in.Message.ID == "" {
		return nil
	}
	if sk, ok := h.deps.Sinks[in.Sink]; ok {
		return sk.SetActions(ctx, in.Message, part, actions)
	}
	if in.Sink == sink.Telegram && h.deps.Telegram != nil {
		chatID, _ := strconv.ParseInt(in.Message.Chat, 10, 64)
		msgID, _ := strconv.ParseInt(in.Message.ID, 10, 64)
		return h.deps.Telegram.EditMessageReplyMarkup(ctx, chatID, msgID, telegram.Keyboard(actions))
	}
	return nil
}

func (h *CallbackHandler) answerTelegram(ctx context.Context, id, text string, showAlert bool) {
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

func int64Strings(in []int64) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.FormatInt(v, 10)
	}
	return out
}

// createdBy is the Alertmanager silence author: sink plus username (or ID).
func createdBy(sinkName string, u Actor) string {
	if u.Name != "" {
		return sinkName + ":@" + u.Name
	}
	return sinkName + ":" + u.ID
}

func silenceCreatedBy(u telegram.User) string {
	return createdBy(sink.Telegram, Actor{ID: strconv.FormatInt(u.ID, 10), Name: u.Username})
}

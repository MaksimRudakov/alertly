package server

import (
	"log/slog"
	"strconv"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
)

// maxCallbackDataBytes is the Telegram Bot API limit for
// InlineKeyboardButton.callback_data. Exceeding it fails the whole
// sendMessage, not just the button.
const maxCallbackDataBytes = 64

// AlertmanagerKeyboard attaches a single row of silence buttons to firing
// alertmanager notifications in allowlisted chats (Telegram) and channels
// (Slack) of clusters that have an Alertmanager. It also populates the label
// cache so callbacks can resolve labels even if AM no longer has the alert.
type AlertmanagerKeyboard struct {
	Durations     []string // ordered, e.g. ["1h", "4h", "24h"]
	ChatAllowlist []int64
	// SlackChannels get buttons too (Slack interactivity); empty = none.
	SlackChannels []string
	Cache         *alertmanager.LabelCache
	Logger        *slog.Logger
}

func (k *AlertmanagerKeyboard) Build(cluster *Cluster, target sink.Target, n notification.Notification, sourceName string) *sink.Actions {
	if k == nil || sourceName != "alertmanager" || cluster == nil || cluster.AM == nil {
		return nil
	}
	if n.Status != "firing" || n.Fingerprint == "" || !k.allowed(target) {
		return nil
	}

	// Cache labels on the way out so the callback handler has a fallback if AM
	// already forgot about the alert. Happens per target, but Put is idempotent.
	k.Cache.Put(labelCacheKey(cluster.Name, n.Fingerprint), n.Labels)

	row := make([]sink.Button, 0, len(k.Durations))
	for _, d := range k.Durations {
		data := buildSilenceData(cluster, n.Fingerprint, d)
		if len(data) > maxCallbackDataBytes {
			if k.Logger != nil {
				k.Logger.Warn("silence button skipped: callback_data exceeds Telegram limit",
					"fingerprint", n.Fingerprint,
					"duration", d,
					"bytes", len(data),
					"limit", maxCallbackDataBytes,
				)
			}
			continue
		}
		row = append(row, sink.Button{Text: "🔇 Silence " + d, Data: data})
	}
	if len(row) == 0 {
		return nil
	}
	return &sink.Actions{Rows: [][]sink.Button{row}}
}

func (k *AlertmanagerKeyboard) allowed(t sink.Target) bool {
	switch t.Sink {
	case sink.Telegram:
		chatID, err := strconv.ParseInt(t.Chat, 10, 64)
		return err == nil && int64InSet(chatID, k.ChatAllowlist)
	case sink.Slack:
		return stringInSet(t.Chat, k.SlackChannels)
	}
	return false
}

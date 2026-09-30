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
// alertmanager notifications in allowlisted Telegram chats of clusters that
// have an Alertmanager. It also populates the label cache so callbacks can
// resolve labels even if AM no longer has the alert.
type AlertmanagerKeyboard struct {
	Durations     []string // ordered, e.g. ["1h", "4h", "24h"]
	ChatAllowlist []int64
	Cache         *alertmanager.LabelCache
	Logger        *slog.Logger
}

func (k *AlertmanagerKeyboard) Build(cluster *Cluster, target sink.Target, n notification.Notification, sourceName string) *sink.Actions {
	if k == nil || sourceName != "alertmanager" || cluster == nil || cluster.AM == nil {
		return nil
	}
	if target.Sink != sink.Telegram || n.Status != "firing" || n.Fingerprint == "" {
		return nil
	}
	chatID, err := strconv.ParseInt(target.Chat, 10, 64)
	if err != nil || !int64InSet(chatID, k.ChatAllowlist) {
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

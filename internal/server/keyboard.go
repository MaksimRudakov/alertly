package server

import (
	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// AlertmanagerKeyboard attaches a single row of silence buttons to firing
// alertmanager notifications in allowlisted chats. It also populates the label
// cache so callbacks can resolve labels even if AM no longer has the alert.
type AlertmanagerKeyboard struct {
	Durations     []string // ordered, e.g. ["1h", "4h", "24h"]
	ChatAllowlist []int64
	Cache         *alertmanager.LabelCache
}

func (k *AlertmanagerKeyboard) Build(target notification.ChatTarget, n notification.Notification, sourceName string) *telegram.SendOptions {
	if k == nil || sourceName != "alertmanager" {
		return nil
	}
	if n.Status != "firing" || n.Fingerprint == "" {
		return nil
	}
	if !int64InSet(target.ChatID, k.ChatAllowlist) {
		return nil
	}

	// Cache labels on the way out so the callback handler has a fallback if AM
	// already forgot about the alert. Happens per target, but Put is idempotent.
	k.Cache.Put(n.Fingerprint, n.Labels)

	silenceRow := make([]telegram.InlineKeyboardButton, 0, len(k.Durations))
	for _, d := range k.Durations {
		silenceRow = append(silenceRow, telegram.InlineKeyboardButton{
			Text:         "🔇 Silence " + d,
			CallbackData: BuildCallbackData(CallbackActionSilence, n.Fingerprint, d),
		})
	}
	return &telegram.SendOptions{
		ReplyMarkup: &telegram.InlineKeyboardMarkup{
			InlineKeyboard: [][]telegram.InlineKeyboardButton{silenceRow},
		},
	}
}

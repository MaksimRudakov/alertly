package server

import (
	"log/slog"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// maxCallbackDataBytes is the Telegram Bot API limit for
// InlineKeyboardButton.callback_data. Exceeding it fails the whole
// sendMessage, not just the button.
const maxCallbackDataBytes = 64

// AlertmanagerKeyboard attaches a single row of silence buttons to firing
// alertmanager notifications in allowlisted chats. It also populates the label
// cache so callbacks can resolve labels even if AM no longer has the alert.
type AlertmanagerKeyboard struct {
	Durations     []string // ordered, e.g. ["1h", "4h", "24h"]
	ChatAllowlist []int64
	Cache         *alertmanager.LabelCache
	Logger        *slog.Logger
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
		data := BuildCallbackData(CallbackActionSilence, n.Fingerprint, d)
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
		silenceRow = append(silenceRow, telegram.InlineKeyboardButton{
			Text:         "🔇 Silence " + d,
			CallbackData: data,
		})
	}
	if len(silenceRow) == 0 {
		return nil
	}
	return &telegram.SendOptions{
		ReplyMarkup: &telegram.InlineKeyboardMarkup{
			InlineKeyboard: [][]telegram.InlineKeyboardButton{silenceRow},
		},
	}
}

package server

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

func parseChatTargets(raw string) ([]notification.ChatTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty chat list")
	}
	parts := strings.Split(raw, ",")
	out := make([]notification.ChatTarget, 0, len(parts))
	for _, p := range parts {
		t, err := parseSingleTarget(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func parseSingleTarget(s string) (notification.ChatTarget, error) {
	if s == "" {
		return notification.ChatTarget{}, fmt.Errorf("empty chat id")
	}
	chatStr, threadStr, hasThread := strings.Cut(s, ":")
	chatID, err := strconv.ParseInt(chatStr, 10, 64)
	if err != nil {
		return notification.ChatTarget{}, fmt.Errorf("invalid chat id %q: %w", chatStr, err)
	}
	t := notification.ChatTarget{ChatID: chatID}
	if hasThread {
		threadID, err := strconv.Atoi(threadStr)
		if err != nil {
			return notification.ChatTarget{}, fmt.Errorf("invalid thread id %q: %w", threadStr, err)
		}
		t.ThreadID = &threadID
	}
	return t, nil
}

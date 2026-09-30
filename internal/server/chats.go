package server

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/MaksimRudakov/alertly/internal/sink"
)

// parseChatTargets parses the {chats} segment of the legacy route: a
// comma-separated list of targets. A bare `chat_id[:thread_id]` is Telegram
// (the pre-Slack format); `tg:` and `slack:` prefixes name the sink
// explicitly, e.g. `-100123:42,slack:C0123ABCD`.
func parseChatTargets(raw string) ([]sink.Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty chat list")
	}
	parts := strings.Split(raw, ",")
	out := make([]sink.Target, 0, len(parts))
	for _, p := range parts {
		t, err := parseSingleTarget(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func parseSingleTarget(s string) (sink.Target, error) {
	if s == "" {
		return sink.Target{}, fmt.Errorf("empty chat id")
	}
	if rest, ok := strings.CutPrefix(s, "slack:"); ok {
		return parseSlackTarget(rest)
	}
	s = strings.TrimPrefix(s, "tg:")
	return parseTelegramTarget(s)
}

func parseTelegramTarget(s string) (sink.Target, error) {
	chatStr, threadStr, hasThread := strings.Cut(s, ":")
	if _, err := strconv.ParseInt(chatStr, 10, 64); err != nil {
		return sink.Target{}, fmt.Errorf("invalid chat id %q: %w", chatStr, err)
	}
	t := sink.Target{Sink: sink.Telegram, Chat: chatStr}
	if hasThread {
		if _, err := strconv.Atoi(threadStr); err != nil {
			return sink.Target{}, fmt.Errorf("invalid thread id %q: %w", threadStr, err)
		}
		t.Thread = threadStr
	}
	return t, nil
}

func parseSlackTarget(s string) (sink.Target, error) {
	channel, thread, _ := strings.Cut(s, ":")
	if !isSlackChannelID(channel) {
		return sink.Target{}, fmt.Errorf("invalid slack channel %q: want a channel ID (C…/G…/D…)", channel)
	}
	return sink.Target{Sink: sink.Slack, Chat: channel, Thread: thread}, nil
}

func isSlackChannelID(s string) bool {
	if len(s) < 9 || (s[0] != 'C' && s[0] != 'G' && s[0] != 'D') {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

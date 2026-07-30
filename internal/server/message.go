package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// CommandDeps carries dependencies for handling chat-ops command messages.
type CommandDeps struct {
	Logger        *slog.Logger
	Telegram      telegram.Client
	ChatAllowlist []int64
	UserAllowlist []int64
	Status        *StatusReporter
}

// MessageHandler processes incoming Telegram messages from the long-poll loop
// and answers read-only chat-ops commands. Only /status is supported; anything
// else is ignored so the bot stays silent on unrelated group chatter.
type MessageHandler struct {
	deps CommandDeps
}

func NewMessageHandler(deps CommandDeps) *MessageHandler {
	return &MessageHandler{deps: deps}
}

// Handle processes one message. Errors are logged, never propagated — the
// long-poll loop must keep running.
func (h *MessageHandler) Handle(ctx context.Context, msg *telegram.Message) {
	if msg == nil || msg.Text == "" {
		return
	}
	cmd := parseCommand(msg.Text)
	if cmd == "" {
		return
	}

	logger := h.deps.Logger.With("chat_id", msg.Chat.ID, "command", cmd)
	if msg.From != nil {
		logger = logger.With("user_id", msg.From.ID, "username", msg.From.Username)
	}

	if cmd != "status" {
		// Groups deliver every /command to every bot; stay silent instead of
		// spamming "unknown command" for commands addressed to other bots.
		metrics.CommandsReceived.WithLabelValues("unknown", "ignored").Inc()
		logger.Debug("command: unknown, ignored")
		return
	}

	if !int64InSet(msg.Chat.ID, h.deps.ChatAllowlist) {
		metrics.CommandsReceived.WithLabelValues(cmd, "auth_failed").Inc()
		logger.Warn("command: chat not in allowlist")
		return
	}
	if len(h.deps.UserAllowlist) > 0 && (msg.From == nil || !int64InSet(msg.From.ID, h.deps.UserAllowlist)) {
		metrics.CommandsReceived.WithLabelValues(cmd, "auth_failed").Inc()
		logger.Warn("command: user not in allowlist")
		h.reply(ctx, msg, "⛔ You are not authorized to run commands.", logger)
		return
	}

	if h.reply(ctx, msg, h.deps.Status.Text(), logger) {
		metrics.CommandsReceived.WithLabelValues(cmd, "ok").Inc()
		logger.Info("command handled")
	} else {
		metrics.CommandsReceived.WithLabelValues(cmd, "send_error").Inc()
	}
}

func (h *MessageHandler) reply(ctx context.Context, msg *telegram.Message, text string, logger *slog.Logger) bool {
	var threadID *int
	if msg.IsTopicMessage {
		threadID = msg.MessageThreadID
	}
	if _, err := h.deps.Telegram.SendMessage(ctx, msg.Chat.ID, threadID, text, nil); err != nil {
		logger.Error("command: send reply failed", "err", err)
		return false
	}
	return true
}

// parseCommand extracts the bot-command name from a message: "/status@mybot
// extra" -> "status". Returns "" for non-command messages.
func parseCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd)
}

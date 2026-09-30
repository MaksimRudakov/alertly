package server

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

// CommandDeps carries dependencies for handling chat-ops commands.
type CommandDeps struct {
	Logger        *slog.Logger
	Telegram      telegram.Client
	ChatAllowlist []int64
	UserAllowlist []int64
	// Access per sink; nil = Telegram only, from ChatAllowlist/UserAllowlist.
	Access map[string]AccessPolicy
	Status *StatusReporter
	// Clusters by name; nil = single-cluster mode (the reporter's own AM).
	Clusters map[string]*Cluster
}

// Command is a chat-ops command from any messenger.
type Command struct {
	Sink   string
	Chat   string
	Thread string // reply target: Telegram forum topic, Slack thread
	User   Actor
	Name   string   // "status"
	Args   []string // e.g. ["k8s-prod"]
}

// CommandReplier answers a command in its messenger.
type CommandReplier interface {
	Status(ctx context.Context, s StatusSnapshot) error
	// Deny reports a refusal; chatLevel marks a non-allowlisted chat, where
	// Telegram stays silent (groups deliver every /command to every bot).
	Deny(ctx context.Context, text string, chatLevel bool) error
}

// MessageHandler answers read-only chat-ops commands. Only status is
// supported; anything else is ignored so the bot stays silent on unrelated
// group chatter.
type MessageHandler struct {
	deps CommandDeps
}

func NewMessageHandler(deps CommandDeps) *MessageHandler {
	if deps.Access == nil {
		deps.Access = map[string]AccessPolicy{sink.Telegram: {
			Chats: int64Strings(deps.ChatAllowlist),
			Users: int64Strings(deps.UserAllowlist),
		}}
	}
	return &MessageHandler{deps: deps}
}

// Handle adapts a Telegram message. Errors are logged, never propagated —
// the long-poll loop must keep running.
func (h *MessageHandler) Handle(ctx context.Context, msg *telegram.Message) {
	if msg == nil || msg.Text == "" {
		return
	}
	name, args := parseCommandArgs(msg.Text)
	if name == "" {
		return
	}
	cmd := Command{
		Sink: sink.Telegram,
		Chat: strconv.FormatInt(msg.Chat.ID, 10),
		Name: name,
		Args: args,
	}
	if msg.From != nil {
		cmd.User = Actor{ID: strconv.FormatInt(msg.From.ID, 10), Name: msg.From.Username}
	}
	h.HandleCommand(ctx, cmd, &telegramReplier{h: h, msg: msg})
}

// HandleCommand processes one command from any messenger.
func (h *MessageHandler) HandleCommand(ctx context.Context, cmd Command, reply CommandReplier) {
	logger := h.deps.Logger.With("sink", cmd.Sink, "chat", cmd.Chat, "command", cmd.Name, "user_id", cmd.User.ID, "username", cmd.User.Name)
	count := func(command, status string) {
		metrics.CommandsReceived.WithLabelValues(command, status, cmd.Sink).Inc()
	}

	if cmd.Name != "status" {
		count("unknown", "ignored")
		logger.Debug("command: unknown, ignored")
		return
	}

	policy := h.deps.Access[cmd.Sink]
	if !stringInSet(cmd.Chat, policy.Chats) {
		count(cmd.Name, "auth_failed")
		logger.Warn("command: chat not in allowlist")
		if err := reply.Deny(ctx, "⛔ This chat is not allowed to run alertly commands.", true); err != nil {
			logger.Error("command: send reply failed", "err", err)
		}
		return
	}
	if len(policy.Users) > 0 && !stringInSet(cmd.User.ID, policy.Users) {
		count(cmd.Name, "auth_failed")
		logger.Warn("command: user not in allowlist")
		if err := reply.Deny(ctx, "⛔ You are not authorized to run commands.", false); err != nil {
			logger.Error("command: send reply failed", "err", err)
		}
		return
	}

	clusters, ok := h.selectClusters(cmd)
	if !ok {
		count(cmd.Name, "invalid")
		if err := reply.Deny(ctx, "⚠️ Unknown cluster "+strings.Join(cmd.Args, " ")+".", false); err != nil {
			logger.Error("command: send reply failed", "err", err)
		}
		return
	}

	if err := reply.Status(ctx, h.deps.Status.Snapshot(ctx, clusters)); err != nil {
		count(cmd.Name, "send_error")
		logger.Error("command: send reply failed", "err", err)
		return
	}
	count(cmd.Name, "ok")
	logger.Info("command handled")
}

// selectClusters picks the clusters a status reply covers. A chat sees only
// the clusters whose destinations route to it (or the default cluster when
// none do); an argument (name or alias) narrows that set to one cluster and
// cannot reach beyond it — another cluster answers like an unknown one, so a
// chat learns nothing about clusters it does not receive alerts from.
// nil = single-cluster mode.
func (h *MessageHandler) selectClusters(cmd Command) ([]*Cluster, bool) {
	if h.deps.Clusters == nil {
		// Single-cluster mode ignores arguments, as before clusters existed.
		return nil, true
	}
	var bound []*Cluster
	for _, c := range sortedClusters(h.deps.Clusters) {
		if c.routesTo(cmd.Sink, cmd.Chat) {
			bound = append(bound, c)
		}
	}
	if len(bound) == 0 {
		if d := h.deps.Clusters["default"]; d != nil {
			bound = []*Cluster{d}
		}
	}
	if len(cmd.Args) == 0 {
		return bound, true
	}
	for _, c := range bound {
		if c.Name == cmd.Args[0] || c.Alias == cmd.Args[0] {
			return []*Cluster{c}, true
		}
	}
	return nil, false
}

type telegramReplier struct {
	h   *MessageHandler
	msg *telegram.Message
}

func (r *telegramReplier) Status(ctx context.Context, s StatusSnapshot) error {
	return r.send(ctx, FormatStatusTelegram(s))
}

func (r *telegramReplier) Deny(ctx context.Context, text string, chatLevel bool) error {
	if chatLevel {
		return nil
	}
	return r.send(ctx, text)
}

func (r *telegramReplier) send(ctx context.Context, text string) error {
	var threadID *int
	if r.msg.IsTopicMessage {
		threadID = r.msg.MessageThreadID
	}
	_, err := r.h.deps.Telegram.SendMessage(ctx, r.msg.Chat.ID, threadID, text, nil)
	return err
}

// parseCommand extracts the bot-command name from a message: "/status@mybot
// extra" -> "status". Returns "" for non-command messages.
func parseCommand(text string) string {
	name, _ := parseCommandArgs(text)
	return name
}

// parseCommandArgs is parseCommand plus the whitespace-separated arguments.
func parseCommandArgs(text string) (string, []string) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", nil
	}
	cmd := strings.TrimPrefix(fields[0], "/")
	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}
	return strings.ToLower(cmd), fields[1:]
}

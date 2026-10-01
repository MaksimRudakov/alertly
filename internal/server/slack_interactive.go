package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/slack"
)

// SlackInteractive turns Socket Mode envelopes into button presses
// (block_actions) and chat-ops commands (slash commands).
type SlackInteractive struct {
	Socket interface {
		Run(ctx context.Context, handle func(context.Context, slack.Envelope))
	}
	Client    slack.Client
	Callbacks *CallbackHandler
	// Commands answers `/alertly status`; nil disables commands.
	Commands *MessageHandler
	// Command is the slash command registered for the app, e.g. "/alertly".
	Command       string
	Logger        *slog.Logger
	HandleTimeout time.Duration
}

func (s *SlackInteractive) Run(ctx context.Context) {
	s.Socket.Run(ctx, s.dispatch)
}

// dispatch handles one envelope under a timeout and a panic guard: the
// socket worker runs outside the HTTP stack, like the Telegram poller.
func (s *SlackInteractive) dispatch(ctx context.Context, env slack.Envelope) {
	timeout := s.HandleTimeout
	if timeout <= 0 {
		timeout = callbackHandleTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			s.Logger.Error("panic in slack envelope handler recovered",
				"type", env.Type, "panic", rec, "stack", string(debug.Stack()))
		}
	}()
	switch env.Type {
	case "interactive":
		s.interactive(hctx, env.Payload)
	case "slash_commands":
		s.slashCommand(hctx, env.Payload)
	default:
		s.Logger.Debug("slack envelope ignored", "type", env.Type)
	}
}

type blockActionsPayload struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string            `json:"ts"`
		ThreadTS string            `json:"thread_ts"`
		Text     string            `json:"text"`
		Blocks   []json.RawMessage `json:"blocks"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		BlockID  string `json:"block_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func (s *SlackInteractive) interactive(ctx context.Context, raw json.RawMessage) {
	var p blockActionsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.Logger.Warn("slack interactive: undecodable payload", "err", err)
		return
	}
	if p.Type != "block_actions" || s.Callbacks == nil {
		return
	}
	var value string
	for _, a := range p.Actions {
		if a.BlockID == slack.ActionsBlockID {
			value = a.Value
			break
		}
	}
	if value == "" {
		return // not one of alertly's buttons
	}
	in := Interaction{
		Sink:     sink.Slack,
		Chat:     p.Channel.ID,
		Thread:   p.Message.ThreadTS,
		User:     Actor{ID: p.User.ID, Name: p.User.Username},
		Message:  sink.MessageRef{Sink: sink.Slack, Chat: p.Channel.ID, ID: p.Message.TS},
		Data:     value,
		Original: slack.OriginalPart(p.Message.Text, p.Message.Blocks),
	}
	s.Callbacks.HandleInteraction(ctx, in, func(ctx context.Context, text string, _ bool) {
		// Detached, like Telegram's answer: the user must learn the outcome
		// even when the handle budget went into an Alertmanager call.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Client.PostEphemeral(cctx, in.Chat, in.User.ID, in.Thread, text); err != nil {
			s.Logger.Warn("slack: ephemeral feedback failed", "err", err)
		}
	})
}

type slashCommandPayload struct {
	Command     string `json:"command"`
	Text        string `json:"text"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	ResponseURL string `json:"response_url"`
}

const slackCommandUsage = "Usage: `%s status [cluster]`"

func (s *SlackInteractive) slashCommand(ctx context.Context, raw json.RawMessage) {
	var p slashCommandPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		s.Logger.Warn("slack slash command: undecodable payload", "err", err)
		return
	}
	if p.Command != s.Command {
		return
	}
	reply := &slackReplier{client: s.Client, responseURL: p.ResponseURL}
	fields := strings.Fields(p.Text)
	name := "status"
	if len(fields) > 0 {
		name, fields = strings.ToLower(fields[0]), fields[1:]
	}
	if s.Commands == nil || name != "status" {
		_ = reply.Deny(ctx, strings.Replace(slackCommandUsage, "%s", s.Command, 1), false)
		return
	}
	s.Commands.HandleCommand(ctx, Command{
		Sink: sink.Slack,
		Chat: p.ChannelID,
		User: Actor{ID: p.UserID, Name: p.UserName},
		Name: name,
		Args: fields,
	}, reply)
}

// slackReplier answers through the slash command's response_url: status is
// posted to the channel (visible to everyone, like Telegram), refusals only
// to the caller.
type slackReplier struct {
	client      slack.Client
	responseURL string
}

func (r *slackReplier) Status(ctx context.Context, s StatusSnapshot) error {
	return r.client.Respond(ctx, r.responseURL, slack.CommandReply{ResponseType: "in_channel", Text: FormatStatusSlack(s)})
}

func (r *slackReplier) Deny(ctx context.Context, text string, _ bool) error {
	return r.client.Respond(ctx, r.responseURL, slack.CommandReply{ResponseType: "ephemeral", Text: text})
}

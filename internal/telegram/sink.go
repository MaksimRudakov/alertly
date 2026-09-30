package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// Sink adapts Client to sink.Sink: HTML rendering (operator template or the
// builtin layout), splitting to TelegramTextLimit, inline keyboards.
type Sink struct {
	client   Client
	renderer tmpl.Renderer
	builtin  bool
	labels   []string
}

// NewSink builds the Telegram sink. With builtin=true the renderer is not
// consulted and labels selects the key fields of the builtin layout.
func NewSink(c Client, r tmpl.Renderer, builtin bool, labels []string) *Sink {
	return &Sink{client: c, renderer: r, builtin: builtin, labels: labels}
}

func (s *Sink) Name() string { return sink.Telegram }

// Client exposes the transport for Telegram-only features (callbacks,
// commands) that are not part of the sink contract yet.
func (s *Sink) Client() Client { return s.client }

func (s *Sink) Render(templateName string, n notification.Notification) ([]sink.Part, error) {
	var text string
	if s.builtin {
		text = RenderBuiltin(n, s.labels)
	} else {
		rendered, err := s.renderer.Render(templateName, n)
		if err != nil {
			return nil, &sink.RenderError{Template: templateName, Err: err}
		}
		text = rendered
	}
	chunks := SplitMessage(text, TelegramTextLimit)
	if len(chunks) > 1 {
		metrics.MessageSplitTotal.Inc()
	}
	parts := make([]sink.Part, len(chunks))
	for i, c := range chunks {
		parts[i] = sink.Part{Text: c}
	}
	return parts, nil
}

func (s *Sink) Send(ctx context.Context, t sink.Target, p sink.Part, actions *sink.Actions) (sink.MessageRef, error) {
	chatID, threadID, err := ParseTarget(t)
	if err != nil {
		return sink.MessageRef{}, err
	}
	var opts *SendOptions
	if actions != nil && len(actions.Rows) > 0 {
		opts = &SendOptions{ReplyMarkup: Keyboard(actions)}
	}
	msgID, err := s.client.SendMessage(ctx, chatID, threadID, p.Text, opts)
	if err != nil {
		return sink.MessageRef{}, err
	}
	return sink.MessageRef{Sink: sink.Telegram, Chat: t.Chat, ID: strconv.FormatInt(msgID, 10)}, nil
}

func (s *Sink) Probe(ctx context.Context) error { return s.client.GetMe(ctx) }

func (s *Sink) Classify(err error) sink.ErrorClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sink.ErrCanceled
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusTooManyRequests:
			return sink.ErrRateLimited
		case ae.StatusCode >= 500:
			return sink.ErrServer
		default:
			return sink.ErrClient
		}
	}
	return sink.ErrServer
}

// ParseTarget converts a sink target into Telegram chat and optional topic.
func ParseTarget(t sink.Target) (int64, *int, error) {
	chatID, err := strconv.ParseInt(t.Chat, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("telegram target: invalid chat id %q", t.Chat)
	}
	if t.Thread == "" {
		return chatID, nil, nil
	}
	threadID, err := strconv.Atoi(t.Thread)
	if err != nil {
		return 0, nil, fmt.Errorf("telegram target: invalid thread id %q", t.Thread)
	}
	return chatID, &threadID, nil
}

// Keyboard converts messenger-neutral actions into an inline keyboard.
func Keyboard(a *sink.Actions) *InlineKeyboardMarkup {
	if a == nil {
		return nil
	}
	rows := make([][]InlineKeyboardButton, 0, len(a.Rows))
	for _, r := range a.Rows {
		row := make([]InlineKeyboardButton, 0, len(r))
		for _, b := range r {
			row = append(row, InlineKeyboardButton{Text: b.Text, CallbackData: b.Data})
		}
		rows = append(rows, row)
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

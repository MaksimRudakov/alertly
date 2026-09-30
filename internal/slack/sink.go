package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// TemplatePrefix namespaces Slack templates in the shared templates map:
// `slack.alertmanager`, falling back to `slack.default`.
const TemplatePrefix = "slack."

// Sink adapts Client to sink.Sink.
type Sink struct {
	client   Client
	renderer tmpl.Renderer
	builtin  bool
	labels   []string
}

// NewSink builds the Slack sink. builtin=false renders templates named
// TemplatePrefix+<source> (mrkdwn) instead of the builtin Block Kit layout.
func NewSink(c Client, r tmpl.Renderer, builtin bool, labels []string) *Sink {
	return &Sink{client: c, renderer: r, builtin: builtin, labels: labels}
}

func (s *Sink) Name() string { return sink.Slack }

func (s *Sink) Render(templateName string, n notification.Notification) ([]sink.Part, error) {
	var parts []messagePart
	if s.builtin {
		parts = renderBuiltin(n, s.labels)
	} else {
		name := TemplatePrefix + templateName
		if !s.renderer.Has(name) {
			name = TemplatePrefix + tmpl.DefaultName
		}
		text, err := s.renderer.Render(name, n)
		if err != nil {
			return nil, &sink.RenderError{Template: name, Err: err}
		}
		parts = renderTemplate(n, text)
	}
	out := make([]sink.Part, 0, len(parts))
	for _, p := range parts {
		payload, err := json.Marshal(p.payload)
		if err != nil {
			return nil, fmt.Errorf("marshal slack payload: %w", err)
		}
		out = append(out, sink.Part{Text: p.text, Payload: payload})
	}
	return out, nil
}

// Send posts one part, with actions as a trailing Block Kit actions block.
func (s *Sink) Send(ctx context.Context, t sink.Target, p sink.Part, actions *sink.Actions) (sink.MessageRef, error) {
	msg, err := message(t.Chat, p, actions)
	if err != nil {
		return sink.MessageRef{}, err
	}
	msg.ThreadTS = t.Thread
	ts, err := s.client.PostMessage(ctx, msg)
	if err != nil {
		return sink.MessageRef{}, err
	}
	return sink.MessageRef{Sink: sink.Slack, Chat: t.Chat, ID: ts}, nil
}

// SetActions rewrites the message with new buttons (chat.update needs the
// whole body, hence original).
func (s *Sink) SetActions(ctx context.Context, ref sink.MessageRef, original sink.Part, actions *sink.Actions) error {
	msg, err := message(ref.Chat, original, actions)
	if err != nil {
		return err
	}
	return s.client.UpdateMessage(ctx, ref.ID, msg)
}

// Client exposes the transport for interactive features.
func (s *Sink) Client() Client { return s.client }

func message(channel string, p sink.Part, actions *sink.Actions) (Message, error) {
	msg := Message{Channel: channel, Text: p.Text}
	if len(p.Payload) > 0 {
		var pp partPayload
		if err := json.Unmarshal(p.Payload, &pp); err != nil {
			return Message{}, fmt.Errorf("slack payload: %w", err)
		}
		msg.Attachments = pp.Attachments
	}
	if block := actionsBlock(actions); block != nil {
		if len(msg.Attachments) == 0 {
			msg.Attachments = []Attachment{{}}
		}
		last := &msg.Attachments[len(msg.Attachments)-1]
		last.Blocks = append(append([]json.RawMessage(nil), last.Blocks...), block)
	}
	return msg, nil
}

// ActionsBlockID marks the block alertly owns in a message.
const ActionsBlockID = "alertly_actions"

// actionsBlock renders buttons; value carries the same callback data format
// as Telegram, so one parser serves both messengers.
func actionsBlock(a *sink.Actions) json.RawMessage {
	if a == nil {
		return nil
	}
	var elements []any
	for _, row := range a.Rows {
		for _, b := range row {
			elements = append(elements, map[string]any{
				"type":      "button",
				"action_id": fmt.Sprintf("alertly_%d", len(elements)),
				"text":      map[string]any{"type": "plain_text", "text": b.Text, "emoji": true},
				"value":     b.Data,
			})
		}
	}
	if len(elements) == 0 {
		return nil
	}
	return block(map[string]any{"type": "actions", "block_id": ActionsBlockID, "elements": elements})
}

func (s *Sink) Probe(ctx context.Context) error { return s.client.AuthTest(ctx) }

func (s *Sink) Classify(err error) sink.ErrorClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sink.ErrCanceled
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusTooManyRequests || ae.Code == "ratelimited":
			return sink.ErrRateLimited
		case ae.StatusCode >= 500 || serverSideCodes[ae.Code]:
			return sink.ErrServer
		default:
			return sink.ErrClient
		}
	}
	return sink.ErrServer
}

// OriginalPart rebuilds a sink.Part from a message as Slack reports it in an
// interaction payload, minus alertly's actions block — what SetActions needs
// when the tracker no longer holds the message as sent.
func OriginalPart(text string, attachments []Attachment) sink.Part {
	clean := make([]Attachment, 0, len(attachments))
	for _, a := range attachments {
		kept := a.Blocks[:0:0]
		for _, b := range a.Blocks {
			var meta struct {
				BlockID string `json:"block_id"`
			}
			if json.Unmarshal(b, &meta) == nil && meta.BlockID == ActionsBlockID {
				continue
			}
			kept = append(kept, b)
		}
		a.Blocks = kept
		clean = append(clean, a)
	}
	payload, _ := json.Marshal(partPayload{Attachments: clean})
	return sink.Part{Text: text, Payload: payload}
}

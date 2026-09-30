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

// Send posts one part. Actions are ignored until Slack interactivity ships.
func (s *Sink) Send(ctx context.Context, t sink.Target, p sink.Part, _ *sink.Actions) (sink.MessageRef, error) {
	msg := Message{Channel: t.Chat, ThreadTS: t.Thread, Text: p.Text}
	if len(p.Payload) > 0 {
		var pp partPayload
		if err := json.Unmarshal(p.Payload, &pp); err != nil {
			return sink.MessageRef{}, fmt.Errorf("slack payload: %w", err)
		}
		msg.Attachments = pp.Attachments
	}
	ts, err := s.client.PostMessage(ctx, msg)
	if err != nil {
		return sink.MessageRef{}, err
	}
	return sink.MessageRef{Sink: sink.Slack, Chat: t.Chat, ID: ts}, nil
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

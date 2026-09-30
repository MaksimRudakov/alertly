// Package sink abstracts the messengers alertly delivers to. The webhook
// handler renders, sends and classifies errors through this interface; each
// messenger package (telegram, slack) owns its formatting, splitting and
// transport.
package sink

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

// Names of the built-in sinks.
const (
	Telegram = "telegram"
	Slack    = "slack"
)

// Target is one delivery address. Chat and Thread are strings so both
// Telegram (numeric chat/topic IDs) and Slack (channel ID, thread_ts) fit.
type Target struct {
	Sink   string
	Chat   string
	Thread string // "" = no thread/topic
}

func (t Target) String() string {
	if t.Thread == "" {
		return t.Sink + ":" + t.Chat
	}
	return t.Sink + ":" + t.Chat + ":" + t.Thread
}

// MessageRef identifies a delivered message (Telegram message_id, Slack ts).
type MessageRef struct {
	Sink string
	Chat string
	ID   string
}

// Part is one sendable message. Text is always set (Telegram body, Slack
// notification fallback); Payload carries sink-specific structure (Slack
// blocks/attachments) and is nil for plain-text sinks.
type Part struct {
	Text    string
	Payload json.RawMessage
}

// Button is a messenger-neutral inline button: Data is the opaque callback
// payload the interactive handler parses.
type Button struct {
	Text string
	Data string
}

// Actions is a keyboard attached to the last part of a message.
type Actions struct {
	Rows [][]Button
}

// ErrorClass drives readiness accounting of a failed send.
type ErrorClass int

const (
	// ErrClient: the request was wrong (bad chat, bad markup) — not the
	// messenger's fault, never degrades readiness.
	ErrClient ErrorClass = iota
	// ErrServer: 5xx or network — sustained, it flips the sink unready.
	ErrServer
	// ErrRateLimited: backpressure, not an outage.
	ErrRateLimited
	// ErrCanceled: our own deadline or shutdown.
	ErrCanceled
)

// Sink delivers rendered notifications to one messenger.
type Sink interface {
	Name() string
	// Render formats n (template name = source name, with the renderer's own
	// fallback) and splits the result into parts that fit the messenger.
	Render(templateName string, n notification.Notification) ([]Part, error)
	// Send delivers one part; actions (nil = none) go on this part.
	Send(ctx context.Context, t Target, p Part, actions *Actions) (MessageRef, error)
	// Probe checks credentials and reachability (getMe / auth.test).
	Probe(ctx context.Context) error
	Classify(err error) ErrorClass
}

// RenderError marks a formatting failure so the handler can meter it
// separately from delivery errors.
type RenderError struct {
	Template string
	Err      error
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("render template %q: %v", e.Template, e.Err)
}

func (e *RenderError) Unwrap() error { return e.Err }

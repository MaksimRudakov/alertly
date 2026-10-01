package slack

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/MaksimRudakov/alertly/internal/notification"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// Block Kit limits (https://docs.slack.dev/reference/block-kit/blocks).
const (
	headerTextLimit  = 150
	sectionTextLimit = 3000
	fieldTextLimit   = 2000
	maxFields        = 10
	// maxBlocksPerMessage stays under Slack's 50 with room for the header
	// and trailing field/link blocks of the last part.
	maxBlocksPerMessage = 45
	fallbackTextLimit   = 300
)

// partPayload is the Slack-specific part of a message carried in
// sink.Part.Payload; channel and thread are added at send time. Blocks are
// top-level (not inside a legacy attachment): Slack collapses attachments
// behind "Show more" after a few lines, which hid the silence buttons.
type partPayload struct {
	Blocks []json.RawMessage `json:"blocks"`
}

// EscapeMrkdwn escapes the three characters Slack's mrkdwn treats as control
// characters. Everything else (*, _, ~, `) is left to the author.
func EscapeMrkdwn(s string) string {
	return mrkdwnEscaper.Replace(s)
}

var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// severityShortcode renders the severity emoji as a Slack shortcode: a
// unicode emoji with a variation selector (ℹ️) can show as a blank box in a
// plain_text header, shortcodes always render.
func severityShortcode(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "crit", "fatal", "emergency":
		return ":fire:"
	case "warning", "warn":
		return ":warning:"
	default:
		return ":information_source:"
	}
}

// fallbackText is the top-level `text`: with blocks Slack does not display
// it, but uses it for notifications and screen readers.
func fallbackText(n notification.Notification) string {
	return truncateRunes(tmpl.SeverityEmoji(n.Severity)+" "+n.Title+" — "+strings.Join(n.StatusLine(), " · "), fallbackTextLimit)
}

// renderBuiltin is alertly's own Slack layout, the Block Kit twin of the
// Telegram builtin layout: header, status context, body, key labels, links.
// Long bodies spill into continuation messages.
func renderBuiltin(n notification.Notification, labels []string) []messagePart {
	head := []json.RawMessage{
		block(map[string]any{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": truncateRunes(severityShortcode(n.Severity)+" "+n.Title, headerTextLimit), "emoji": true},
		}),
		block(map[string]any{
			"type":     "context",
			"elements": []any{map[string]any{"type": "mrkdwn", "text": EscapeMrkdwn(strings.Join(n.StatusLine(), " · "))}},
		}),
	}

	var tail []json.RawMessage
	var fields []any
	for _, key := range labels {
		if v := n.Labels[key]; v != "" && len(fields) < maxFields {
			fields = append(fields, map[string]any{
				"type": "mrkdwn",
				"text": truncateRunes("*"+EscapeMrkdwn(key)+"*\n`"+EscapeMrkdwn(v)+"`", fieldTextLimit),
			})
		}
	}
	if len(fields) > 0 {
		tail = append(tail, block(map[string]any{"type": "section", "fields": fields}))
	}
	var links []string
	for _, l := range n.Links {
		links = append(links, "<"+linkURL(l.URL)+"|"+EscapeMrkdwn(l.Title)+">")
	}
	if len(links) > 0 {
		tail = append(tail, block(map[string]any{
			"type":     "context",
			"elements": []any{map[string]any{"type": "mrkdwn", "text": truncateRunes(strings.Join(links, " · "), sectionTextLimit)}},
		}))
	}

	return assemble(n, head, sections(EscapeMrkdwn(strings.TrimSpace(n.Body))), tail)
}

// renderTemplate wraps operator-rendered mrkdwn into section blocks.
func renderTemplate(n notification.Notification, text string) []messagePart {
	return assemble(n, nil, sections(text), nil)
}

type messagePart struct {
	text    string
	payload partPayload
}

// assemble packs head + body sections + tail into as few messages as the
// block limit allows; head goes on the first message, tail on the last.
func assemble(n notification.Notification, head, body, tail []json.RawMessage) []messagePart {
	fallback := fallbackText(n)
	var parts []messagePart
	cur := append([]json.RawMessage(nil), head...)
	flush := func() {
		parts = append(parts, messagePart{text: fallback, payload: partPayload{Blocks: cur}})
		cur = nil
	}
	for _, b := range body {
		if len(cur) >= maxBlocksPerMessage {
			flush()
		}
		cur = append(cur, b)
	}
	if len(cur)+len(tail) > maxBlocksPerMessage {
		flush()
	}
	cur = append(cur, tail...)
	if len(cur) > 0 || len(parts) == 0 {
		flush()
	}
	return parts
}

func sections(text string) []json.RawMessage {
	if text == "" {
		return nil
	}
	var out []json.RawMessage
	for _, chunk := range splitText(text, sectionTextLimit) {
		out = append(out, block(map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": chunk},
		}))
	}
	return out
}

func block(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// linkURL keeps a URL from breaking the <url|text> link syntax.
func linkURL(u string) string {
	return strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C").Replace(u)
}

// splitText cuts text into chunks of at most limit runes, preferring
// paragraph, then line, then word boundaries.
func splitText(text string, limit int) []string {
	var out []string
	for utf8.RuneCountInString(text) > limit {
		cut := byteOffsetAtRune(text, limit)
		idx := -1
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(text[:cut], sep); i > cut/2 {
				idx = i + len(sep)
				break
			}
		}
		if idx <= 0 {
			idx = cut
		}
		out = append(out, strings.TrimRight(text[:idx], "\n "))
		text = strings.TrimLeft(text[idx:], "\n ")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func byteOffsetAtRune(s string, n int) int {
	i := 0
	for pos := range s {
		if i == n {
			return pos
		}
		i++
	}
	return len(s)
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return s[:byteOffsetAtRune(s, limit-1)] + "…"
}

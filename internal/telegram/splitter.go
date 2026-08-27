package telegram

import (
	"strings"
	"unicode/utf8"
)

// TelegramTextLimit is the sendMessage text limit. Telegram counts it in UTF-16
// code units, not runes: every emoji or other astral-plane character costs two.
const TelegramTextLimit = 4096

// tagCloseSlack reserves room for the closing tags a part may need on top of
// the ones already open when it starts. Parts that still overflow after this
// are shrunk by the refinement loop in SplitMessage.
const tagCloseSlack = 32

// maxCutRefinements bounds the shrink loop that makes prefix+head+closing fit
// the limit. Realistic markup converges on the first pass; the cap only keeps
// pathological input (thousands of nested tags) from looping.
const maxCutRefinements = 4

// SplitMessage cuts text into parts that each fit limit UTF-16 code units,
// preferring paragraph, then line, then word boundaries. HTML formatting is
// preserved across the cut: tags left open at the end of a part are closed
// there and reopened at the start of the next one, so no part is rejected by
// Telegram with "can't parse entities: Unclosed start tag".
func SplitMessage(text string, limit int) []string {
	if limit <= 0 {
		limit = TelegramTextLimit
	}
	if utf16Len(text) <= limit {
		return []string{text}
	}

	out := make([]string, 0, 2)
	var open []htmlTag
	for {
		prefix := openingTags(open)
		prefixLen := utf16Len(prefix)
		if prefixLen+utf16Len(text) <= limit {
			out = append(out, prefix+text)
			return out
		}

		budget := limit - prefixLen - utf16Len(closingTags(open)) - tagCloseSlack
		if budget < 1 {
			budget = 1
		}

		var (
			idx     int
			head    string
			closing string
		)
		for i := 0; ; i++ {
			idx = safeCut(text, budget)
			head = strings.TrimRight(text[:idx], "\n ")
			closing = closingTags(scanTags(head, open))
			over := prefixLen + utf16Len(head) + utf16Len(closing) - limit
			if over <= 0 || budget <= 1 || i >= maxCutRefinements {
				break
			}
			budget -= over
			if budget < 1 {
				budget = 1
			}
		}

		if idx <= 0 {
			// Degenerate limit (smaller than a single character): take one rune
			// anyway so the loop always makes progress.
			_, size := utf8.DecodeRuneInString(text)
			idx = size
			head = text[:idx]
			closing = closingTags(scanTags(head, open))
		}

		out = append(out, prefix+head+closing)
		open = scanTags(head, open)
		text = strings.TrimLeft(text[idx:], "\n ")
		if text == "" {
			return out
		}
	}
}

func safeCut(text string, limit int) int {
	maxByte := byteIndexAtUnit(text, limit)
	if maxByte == len(text) {
		return maxByte
	}

	minAcceptable := maxByte / 2

	if i := lastIndexBefore(text, maxByte, "\n\n"); i >= minAcceptable {
		return safeAdjust(text, i+2, maxByte)
	}
	if i := strings.LastIndexByte(text[:maxByte], '\n'); i >= minAcceptable {
		return safeAdjust(text, i+1, maxByte)
	}
	if i := strings.LastIndexByte(text[:maxByte], ' '); i >= minAcceptable {
		return safeAdjust(text, i+1, maxByte)
	}

	return safeAdjust(text, maxByte, maxByte)
}

func safeAdjust(text string, cut, maxByte int) int {
	if cut > maxByte {
		cut = maxByte
	}
	if cut <= 0 {
		cut = maxByte
	}
	return avoidTagSplit(text, cut)
}

func avoidTagSplit(text string, cut int) int {
	prefix := text[:cut]
	open := strings.LastIndexByte(prefix, '<')
	if open < 0 {
		return cut
	}
	close := strings.LastIndexByte(prefix, '>')
	if close > open {
		return cut
	}

	if open == 0 {
		return cut
	}
	return open
}

// formattingTags is the set of tags Telegram's HTML parse mode understands.
// Anything else is treated as plain text, so stray angle brackets in an alert
// body never turn into invented closing tags.
var formattingTags = map[string]bool{
	"b": true, "strong": true,
	"i": true, "em": true,
	"u": true, "ins": true,
	"s": true, "strike": true, "del": true,
	"span": true, "tg-spoiler": true, "tg-emoji": true,
	"a": true, "code": true, "pre": true, "blockquote": true,
}

// htmlTag is an opening tag seen in the text: raw keeps attributes so the tag
// can be reopened verbatim on the next part, name is used to match its close.
type htmlTag struct {
	raw  string
	name string
}

// scanTags replays the tags of s on top of stack and returns the tags still
// open at the end of s. Unbalanced input is tolerated: a stray closing tag with
// no match is ignored, and a closing tag that matches deeper in the stack drops
// everything opened after it (that is what a browser — and Telegram — does).
func scanTags(s string, stack []htmlTag) []htmlTag {
	out := append([]htmlTag(nil), stack...)
	for i := 0; i < len(s); {
		rel := strings.IndexByte(s[i:], '<')
		if rel < 0 {
			break
		}
		start := i + rel
		rel = strings.IndexByte(s[start:], '>')
		if rel < 0 {
			break
		}
		end := start + rel
		raw := s[start : end+1]
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		i = end + 1

		switch {
		case inner == "" || strings.HasPrefix(inner, "!"):
			// Comment or stray marker: not a formatting tag.
		case strings.HasPrefix(inner, "/"):
			name := strings.ToLower(strings.TrimSpace(inner[1:]))
			if !formattingTags[name] {
				continue
			}
			for j := len(out) - 1; j >= 0; j-- {
				if out[j].name == name {
					out = out[:j]
					break
				}
			}
		case strings.HasSuffix(inner, "/"):
			// Self-closing: nothing to reopen.
		default:
			name := tagName(inner)
			if !formattingTags[name] {
				// Not markup Telegram understands — most likely literal text
				// such as "a < b > c". Reopening it would corrupt the message.
				continue
			}
			out = append(out, htmlTag{raw: raw, name: name})
		}
	}
	return out
}

func tagName(inner string) string {
	if i := strings.IndexAny(inner, " \t\n\r"); i >= 0 {
		inner = inner[:i]
	}
	return strings.ToLower(inner)
}

// openingTags renders the stack as it must be reopened on the next part.
func openingTags(stack []htmlTag) string {
	if len(stack) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range stack {
		b.WriteString(t.raw)
	}
	return b.String()
}

// closingTags renders the closers for the stack, innermost first.
func closingTags(stack []htmlTag) string {
	if len(stack) == 0 {
		return ""
	}
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(stack[i].name)
		b.WriteString(">")
	}
	return b.String()
}

// utf16Len counts the text the way Telegram does: in UTF-16 code units, so
// characters outside the BMP (emoji) count as two.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// byteIndexAtUnit returns the byte offset after the first units UTF-16 code
// units of text. A surrogate pair is never cut in half: a rune that would
// straddle the boundary is left for the next part.
func byteIndexAtUnit(text string, units int) int {
	if units <= 0 {
		return 0
	}
	count := 0
	for i, r := range text {
		size := 1
		if r > 0xFFFF { // astral plane: encoded as a surrogate pair
			size = 2
		}
		if count+size > units {
			return i
		}
		count += size
	}
	return len(text)
}

func lastIndexBefore(text string, maxByte int, sub string) int {
	if maxByte > len(text) {
		maxByte = len(text)
	}
	return strings.LastIndex(text[:maxByte], sub)
}

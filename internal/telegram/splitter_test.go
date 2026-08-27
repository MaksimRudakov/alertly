package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitShort(t *testing.T) {
	parts := SplitMessage("hello", 100)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Fatalf("unexpected split: %#v", parts)
	}
}

func TestSplitExactlyAtLimit(t *testing.T) {
	text := strings.Repeat("a", TelegramTextLimit)
	parts := SplitMessage(text, TelegramTextLimit)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
}

func TestSplitOneOverLimit(t *testing.T) {
	text := strings.Repeat("a", TelegramTextLimit+1)
	parts := SplitMessage(text, TelegramTextLimit)
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > TelegramTextLimit {
			t.Fatalf("part exceeds limit: %d", utf8.RuneCountInString(p))
		}
	}
}

func TestSplitOnParagraph(t *testing.T) {
	a := strings.Repeat("a", 30)
	b := strings.Repeat("b", 30)
	c := strings.Repeat("c", 30)
	text := a + "\n\n" + b + "\n\n" + c
	parts := SplitMessage(text, 50)
	if len(parts) < 2 {
		t.Fatalf("expected splits, got %d", len(parts))
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 50 {
			t.Fatalf("part exceeds limit: %s (%d)", p, utf8.RuneCountInString(p))
		}
	}
}

func TestSplitLongUnbroken(t *testing.T) {
	text := strings.Repeat("x", 12000)
	parts := SplitMessage(text, TelegramTextLimit)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	total := 0
	for _, p := range parts {
		total += utf8.RuneCountInString(p)
		if utf8.RuneCountInString(p) > TelegramTextLimit {
			t.Fatalf("part too big")
		}
	}
	if total != 12000 {
		t.Fatalf("lost runes: %d", total)
	}
}

func TestSplitAvoidsMidTagCut(t *testing.T) {
	prefix := strings.Repeat("a", 99)
	text := prefix + "<verylongtagname>x"
	parts := SplitMessage(text, 100)
	if len(parts) < 2 {
		t.Fatalf("want >=2 parts, got %d", len(parts))
	}
	for _, p := range parts {
		opens := strings.Count(p, "<")
		closes := strings.Count(p, ">")
		if opens != closes {
			t.Fatalf("unbalanced angle brackets in part %q (opens=%d closes=%d)", p, opens, closes)
		}
	}
}

func TestSplitUnicode(t *testing.T) {
	text := strings.Repeat("ё", 100)
	parts := SplitMessage(text, 30)
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 30 {
			t.Fatalf("part too big: %d", utf8.RuneCountInString(p))
		}
	}
	if joined := strings.Join(parts, ""); utf8.RuneCountInString(joined) != 100 {
		t.Fatalf("lost unicode runes: %d", utf8.RuneCountInString(joined))
	}
}

// utf16Count mirrors what Telegram counts, independently of the implementation
// under test, so a regression in utf16Len cannot make these assertions vacuous.
func utf16Count(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

func TestSplitCountsUTF16Units(t *testing.T) {
	// 4096 emoji = 4096 runes but 8192 UTF-16 units: Telegram rejects it as
	// "message is too long" unless we split.
	text := strings.Repeat("🔥", TelegramTextLimit)
	parts := SplitMessage(text, TelegramTextLimit)
	if len(parts) < 2 {
		t.Fatalf("want >=2 parts for %d UTF-16 units, got %d", utf16Count(text), len(parts))
	}
	for i, p := range parts {
		if n := utf16Count(p); n > TelegramTextLimit {
			t.Fatalf("part %d exceeds limit: %d units", i, n)
		}
	}
	if joined := strings.Join(parts, ""); joined != text {
		t.Fatalf("lost content: %d runes vs %d", utf8.RuneCountInString(joined), utf8.RuneCountInString(text))
	}
}

func TestSplitNeverCutsSurrogatePair(t *testing.T) {
	// Odd limit forces the cut to land exactly between the halves of a pair.
	parts := SplitMessage(strings.Repeat("🔥", 10), 5)
	for _, p := range parts {
		if !utf8.ValidString(p) {
			t.Fatalf("invalid utf-8 in part %q", p)
		}
		if n := utf16Count(p); n > 5 {
			t.Fatalf("part exceeds limit: %d units", n)
		}
	}
	if joined := strings.Join(parts, ""); joined != strings.Repeat("🔥", 10) {
		t.Fatalf("lost content: %q", joined)
	}
}

// tagBalance reports the tags left open at the end of s (Telegram rejects the
// message with "Unclosed start tag" when this is non-empty).
func tagBalance(t *testing.T, s string) []string {
	t.Helper()
	var stack []string
	for i := 0; i < len(s); {
		open := strings.IndexByte(s[i:], '<')
		if open < 0 {
			break
		}
		open += i
		close := strings.IndexByte(s[open:], '>')
		if close < 0 {
			t.Fatalf("part ends mid-tag: %q", s[open:])
		}
		close += open
		inner := s[open+1 : close]
		i = close + 1
		if strings.HasPrefix(inner, "/") {
			name := strings.TrimPrefix(inner, "/")
			if len(stack) == 0 || stack[len(stack)-1] != name {
				t.Fatalf("closing tag %q does not match stack %v in %q", name, stack, s)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		name := inner
		if sp := strings.IndexByte(name, ' '); sp >= 0 {
			name = name[:sp]
		}
		stack = append(stack, name)
	}
	return stack
}

func TestSplitKeepsHTMLBalanced(t *testing.T) {
	text := "<b>" + strings.Repeat("a", 5000) + "</b>"
	parts := SplitMessage(text, TelegramTextLimit)
	if len(parts) < 2 {
		t.Fatalf("want >=2 parts, got %d", len(parts))
	}
	for i, p := range parts {
		if left := tagBalance(t, p); len(left) != 0 {
			t.Fatalf("part %d leaves tags open: %v (%q…)", i, left, p[:20])
		}
		if n := utf16Count(p); n > TelegramTextLimit {
			t.Fatalf("part %d exceeds limit: %d", i, n)
		}
	}
}

func TestSplitReopensNestedTagsWithAttributes(t *testing.T) {
	body := strings.Repeat("word ", 40)
	text := `<b>head <a href="https://example.com/runbook">` + body + "</a></b>"
	parts := SplitMessage(text, 120)
	if len(parts) < 2 {
		t.Fatalf("want >=2 parts, got %d", len(parts))
	}
	for i, p := range parts {
		if left := tagBalance(t, p); len(left) != 0 {
			t.Fatalf("part %d leaves tags open: %v (%q)", i, left, p)
		}
		if n := utf16Count(p); n > 120 {
			t.Fatalf("part %d exceeds limit: %d (%q)", i, n, p)
		}
	}
	for _, p := range parts[1:] {
		if !strings.HasPrefix(p, `<b><a href="https://example.com/runbook">`) {
			t.Fatalf("continuation part lost its formatting: %q", p)
		}
	}
	// Text content must survive the round-trip: whitespace at a cut point is
	// dropped by design, so compare with all whitespace removed.
	var plain strings.Builder
	for _, p := range parts {
		plain.WriteString(stripTags(p))
	}
	if got, want := dropSpaces(plain.String()), dropSpaces(stripTags(text)); got != want {
		t.Fatalf("content changed:\n got %q\nwant %q", got, want)
	}
}

func stripTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		open := strings.IndexByte(s[i:], '<')
		if open < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+open])
		close := strings.IndexByte(s[i+open:], '>')
		if close < 0 {
			break
		}
		i += open + close + 1
	}
	return b.String()
}

func TestSplitClosesTagOpenedOnlyInFirstPart(t *testing.T) {
	// Tag opens mid-text and closes far past the cut.
	text := strings.Repeat("a", 60) + "<i>" + strings.Repeat("b", 200) + "</i>"
	parts := SplitMessage(text, 100)
	for i, p := range parts {
		if left := tagBalance(t, p); len(left) != 0 {
			t.Fatalf("part %d leaves tags open: %v (%q)", i, left, p)
		}
	}
}

func TestSplitLeavesUnbalancedInputAlone(t *testing.T) {
	// A single unclosed tag in short input is the caller's business, not ours.
	parts := SplitMessage("<b>oops", 100)
	if len(parts) != 1 || parts[0] != "<b>oops" {
		t.Fatalf("unexpected split: %#v", parts)
	}
}

func dropSpaces(s string) string {
	return strings.Join(strings.Fields(s), "")
}

func TestSplitIgnoresNonFormattingAngleBrackets(t *testing.T) {
	// Unescaped comparison in an alert body must not be mistaken for markup.
	text := strings.Repeat("a", 60) + " if x < y > z then " + strings.Repeat("b", 200)
	parts := SplitMessage(text, 100)
	for i, p := range parts {
		if strings.Contains(p, "</") {
			t.Fatalf("part %d invented a closing tag: %q", i, p)
		}
	}
	if joined := dropSpaces(strings.Join(parts, "")); joined != dropSpaces(text) {
		t.Fatalf("content changed: %q", joined)
	}
}

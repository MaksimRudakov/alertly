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

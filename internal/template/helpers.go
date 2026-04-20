package template

import (
	"fmt"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"
)

func defaultFuncMap() template.FuncMap {
	return template.FuncMap{
		"severity_emoji":     SeverityEmoji,
		"escape_html":        EscapeHTML,
		"truncate":           Truncate,
		"join":               JoinStrings,
		"humanize_duration":  HumanizeDuration,
	}
}

func SeverityEmoji(sev string) string {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "critical", "crit", "fatal", "emergency":
		return "🔥"
	case "warning", "warn":
		return "⚠️"
	default:
		return "ℹ️"
	}
}

func EscapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s + "…"
}

func JoinStrings(parts []string, sep string) string {
	return strings.Join(parts, sep)
}

func HumanizeDuration(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		d = -d
		return "in " + humanize(d)
	}
	return humanize(d) + " ago"
}

func humanize(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

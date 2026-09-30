package telegram

import (
	"strings"

	"github.com/MaksimRudakov/alertly/internal/notification"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// RenderBuiltin is alertly's own Telegram layout (format.telegram: builtin).
// It mirrors the Slack builtin layout part for part so an alert reads the
// same in both messengers:
//
//	🔥 <b>Title</b>
//	Firing · cluster k8s-prod · critical
//
//	Body
//
//	alertname: <code>X</code>
//	namespace: <code>Y</code>
//	<a href="…">Runbook</a>
func RenderBuiltin(n notification.Notification, labels []string) string {
	var b strings.Builder
	b.WriteString(tmpl.SeverityEmoji(n.Severity))
	b.WriteString(" <b>")
	b.WriteString(tmpl.EscapeHTML(n.Title))
	b.WriteString("</b>\n")

	b.WriteString(tmpl.EscapeHTML(strings.Join(StatusLine(n), " · ")))

	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString("\n\n")
		b.WriteString(tmpl.EscapeHTML(body))
	}

	var fields []string
	for _, key := range labels {
		if v := n.Labels[key]; v != "" {
			fields = append(fields, tmpl.EscapeHTML(key)+": <code>"+tmpl.EscapeHTML(v)+"</code>")
		}
	}
	for _, l := range n.Links {
		fields = append(fields, `<a href="`+tmpl.EscapeHTML(l.URL)+`">`+tmpl.EscapeHTML(l.Title)+"</a>")
	}
	if len(fields) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(fields, "\n"))
	}
	return b.String()
}

// StatusLine is the "Firing · cluster X · critical" context shared by the
// builtin layouts of every sink.
func StatusLine(n notification.Notification) []string {
	var status string
	switch n.Status {
	case "firing":
		status = "Firing"
	case "resolved":
		status = "Resolved"
	case "":
		status = "Event"
	default:
		status = strings.ToUpper(n.Status[:1]) + n.Status[1:]
	}
	parts := []string{status}
	if n.Cluster != "" {
		parts = append(parts, "cluster "+n.Cluster)
	}
	if n.Severity != "" {
		parts = append(parts, n.Severity)
	}
	return parts
}

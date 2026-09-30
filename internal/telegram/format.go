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

	b.WriteString(tmpl.EscapeHTML(strings.Join(n.StatusLine(), " · ")))

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

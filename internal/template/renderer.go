package template

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/MaksimRudakov/alertly/internal/notification"
)

const DefaultName = "default"

type Renderer interface {
	Render(name string, n notification.Notification) (string, error)
	Has(name string) bool
}

type renderer struct {
	templates map[string]*template.Template
}

func New(templates map[string]string) (Renderer, error) {
	if len(templates) == 0 {
		return nil, fmt.Errorf("no templates configured")
	}
	if _, ok := templates[DefaultName]; !ok {
		return nil, fmt.Errorf("template %q is required", DefaultName)
	}

	r := &renderer{templates: make(map[string]*template.Template, len(templates))}

	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		body := templates[name]
		t, err := template.New(name).Funcs(defaultFuncMap()).Parse(body)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", name, err)
		}
		r.templates[name] = t
	}
	return r, nil
}

func (r *renderer) Has(name string) bool {
	_, ok := r.templates[name]
	return ok
}

func (r *renderer) Render(name string, n notification.Notification) (string, error) {
	t, ok := r.templates[name]
	if !ok {
		t = r.templates[DefaultName]
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, n); err != nil {
		return "", fmt.Errorf("render template %q: %w", name, err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

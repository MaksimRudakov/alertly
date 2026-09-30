package main

import (
	"errors"
	"log/slog"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/sink"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

func newSlackSink(config.Config, tmpl.Renderer, bool, *slog.Logger) (sink.Sink, error) {
	return nil, errors.New("slack sink is not implemented yet")
}

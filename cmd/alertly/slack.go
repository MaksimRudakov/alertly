package main

import (
	"errors"
	"log/slog"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/slack"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

func newSlackSink(cfg config.Config, renderer tmpl.Renderer, dryRun bool, logger *slog.Logger) (sink.Sink, error) {
	token := requireEnv("SLACK_BOT_TOKEN")
	if token == "" {
		return nil, errors.New("SLACK_BOT_TOKEN is required when slack.enabled is true")
	}
	limiter := slack.NewLimiter(cfg.Slack.RateLimit.GlobalPerSec, cfg.Slack.RateLimit.PerChannelPerSec)
	client := slack.New(slack.Config{
		APIURL:         cfg.Slack.APIURL,
		Token:          token,
		RequestTimeout: cfg.Slack.RequestTimeout,
		MaxAttempts:    cfg.Slack.Retry.MaxAttempts,
		InitialBackoff: cfg.Slack.Retry.InitialBackoff,
		MaxBackoff:     cfg.Slack.Retry.MaxBackoff,
		DryRun:         dryRun,
	}, limiter, logger)
	return slack.NewSink(client, renderer, cfg.Format.Slack == config.FormatBuiltin, cfg.Format.Labels), nil
}

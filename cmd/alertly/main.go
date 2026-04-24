package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/server"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
	"github.com/MaksimRudakov/alertly/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg.Logging)
	logger.Info("alertly starting",
		"version", version.Version,
		"commit", version.Commit,
		"date", version.Date,
		"go", version.GoVersion(),
	)

	botToken := requireEnv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		return errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	authToken := requireEnv("WEBHOOK_AUTH_TOKEN")
	if authToken == "" {
		return errors.New("WEBHOOK_AUTH_TOKEN is required")
	}

	registry := metrics.Init()
	metrics.BuildInfo.WithLabelValues(version.Version, version.Commit, version.GoVersion()).Set(1)

	dryRun := config.DryRun()

	limiter := telegram.NewLimiter(cfg.Telegram.RateLimit.GlobalPerSec, cfg.Telegram.RateLimit.PerChatPerSec)
	tgClient := telegram.New(telegram.Config{
		APIURL:         cfg.Telegram.APIURL,
		Token:          botToken,
		ParseMode:      cfg.Telegram.ParseMode,
		RequestTimeout: cfg.Telegram.RequestTimeout,
		MaxAttempts:    cfg.Telegram.Retry.MaxAttempts,
		InitialBackoff: cfg.Telegram.Retry.InitialBackoff,
		MaxBackoff:     cfg.Telegram.Retry.MaxBackoff,
		DryRun:         dryRun,
	}, limiter, logger)

	renderer, err := tmpl.New(cfg.Templates)
	if err != nil {
		return fmt.Errorf("templates: %w", err)
	}

	sources := map[string]source.Source{
		"alertmanager": source.NewAlertmanager(),
		"kubewatch":    source.NewKubewatch(),
	}

	readiness := server.NewReadiness()

	var (
		keyboard   server.KeyboardBuilder
		trackerReg server.ButtonRegistrar
		bgWorkers  []func(context.Context)
	)
	if cfg.Updates.Enabled {
		if dryRun {
			logger.Warn("updates.enabled=true ignored under DRY_RUN")
		} else {
			var err error
			keyboard, trackerReg, bgWorkers, err = setupUpdates(cfg, tgClient, logger)
			if err != nil {
				return fmt.Errorf("updates: %w", err)
			}
		}
	}

	srv := server.New(cfg.Server, server.Deps{
		Logger:    logger,
		Sources:   sources,
		Renderer:  renderer,
		Telegram:  tgClient,
		Readiness: readiness,
		AuthToken: authToken,
		Registry:  registry,
		Keyboard:  keyboard,
		Tracker:   trackerReg,
	})

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if dryRun {
		readiness.MarkReady()
		logger.Warn("DRY_RUN active: telegram calls are skipped")
	} else {
		go startupCheck(rootCtx, tgClient, readiness, logger)
	}

	for _, w := range bgWorkers {
		go w(rootCtx)
	}

	return srv.Run(rootCtx)
}

func setupUpdates(cfg config.Config, tgClient telegram.Client, logger *slog.Logger) (server.KeyboardBuilder, server.ButtonRegistrar, []func(context.Context), error) {
	amCfg := alertmanager.Config{
		URL:            cfg.Alertmanager.URL,
		RequestTimeout: cfg.Alertmanager.RequestTimeout,
		Auth: alertmanager.Auth{
			Username: os.Getenv("ALERTMANAGER_AUTH_USERNAME"),
			Password: os.Getenv("ALERTMANAGER_AUTH_PASSWORD"),
			Token:    os.Getenv("ALERTMANAGER_AUTH_TOKEN"),
		},
	}
	amClient := alertmanager.New(amCfg)

	cache := alertmanager.NewLabelCache(cfg.Updates.LabelCacheTTL, cfg.Updates.LabelCacheMax)
	tracker := server.NewButtonTracker(cfg.Updates.ButtonTTL)

	durations := make(map[string]time.Duration, len(cfg.Updates.SilenceDurations))
	for _, d := range cfg.Updates.SilenceDurations {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse silence duration %q: %w", d, err)
		}
		durations[d] = parsed
	}

	handler := server.NewCallbackHandler(server.CallbackDeps{
		Logger:        logger,
		Telegram:      tgClient,
		AM:            amClient,
		Cache:         cache,
		Tracker:       tracker,
		ChatAllowlist: cfg.Updates.ChatAllowlist,
		UserAllowlist: cfg.Updates.UserAllowlist,
		Durations:     durations,
	})

	keyboard := &server.AlertmanagerKeyboard{
		Durations:     cfg.Updates.SilenceDurations,
		ChatAllowlist: cfg.Updates.ChatAllowlist,
		Cache:         cache,
	}

	poller := &server.UpdatesPoller{
		Client:      tgClient,
		Handler:     handler,
		Logger:      logger,
		PollTimeout: cfg.Updates.PollTimeout,
	}

	sweeper := &server.ButtonSweeper{
		Tracker:  tracker,
		Telegram: tgClient,
		Logger:   logger,
		Interval: time.Minute,
	}

	logger.Info("telegram updates enabled",
		"chat_allowlist", len(cfg.Updates.ChatAllowlist),
		"user_allowlist", len(cfg.Updates.UserAllowlist),
		"durations", cfg.Updates.SilenceDurations,
		"button_ttl", cfg.Updates.ButtonTTL,
		"alertmanager_url", cfg.Alertmanager.URL,
	)
	return keyboard, tracker, []func(context.Context){poller.Run, sweeper.Run}, nil
}

func newLogger(cfg config.Logging) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(cfg.Format) == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func requireEnv(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func startupCheck(ctx context.Context, c telegram.Client, r server.ReadinessTracker, logger *slog.Logger) {
	backoff := time.Second
	maxBackoff := 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.GetMe(callCtx)
		cancel()
		if err == nil {
			logger.Info("telegram getMe ok; readiness=ready")
			r.MarkReady()
			return
		}
		logger.Warn("telegram getMe failed", "err", err, "next_retry_ms", backoff.Milliseconds())
		r.MarkUnready("telegram getMe failed: " + err.Error())

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

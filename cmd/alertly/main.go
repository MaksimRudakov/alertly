package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
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
		"generic":      source.NewGeneric(),
	}

	readiness := server.NewReadiness()

	var dedupCache *dedup.Cache
	if cfg.Dedup.Enabled {
		dedupCache = dedup.New(cfg.Dedup.TTL)
		logger.Info("dedup enabled", "ttl", cfg.Dedup.TTL)
	}

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
		Dedup:     dedupCache,
	})

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	startWorker := func(w func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w(rootCtx)
		}()
	}

	if dryRun {
		readiness.MarkReady()
		logger.Warn("DRY_RUN active: telegram calls are skipped")
	} else {
		startWorker(func(ctx context.Context) { telegramHealthLoop(ctx, tgClient, readiness, logger, time.Minute) })
	}

	for _, w := range bgWorkers {
		startWorker(w)
	}

	if dedupCache != nil {
		startWorker(func(ctx context.Context) { dedupCache.Run(ctx, 0) })
		metrics.RegisterSizeGauge("alertly_dedup_cache_entries",
			"Current number of entries in the dedup cache.", dedupCache.Len)
	}

	err = srv.Run(rootCtx)

	// srv.Run returns after graceful HTTP shutdown (or a listen error). Cancel
	// the workers explicitly for the error path and wait so a callback that is
	// mid-CreateSilence can finish its ack/edit instead of being cut off.
	stop()
	workersDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(cfg.Server.ShutdownTimeout):
		logger.Warn("background workers did not stop within shutdown timeout")
	}
	return err
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
	metrics.RegisterSizeGauge("alertly_label_cache_entries",
		"Current number of fingerprints in the Alertmanager label cache.", cache.Len)
	metrics.RegisterSizeGauge("alertly_button_tracker_entries",
		"Current number of alert messages with active silence buttons.", tracker.Len)

	durations := make(map[string]time.Duration, len(cfg.Updates.SilenceDurations))
	for _, d := range cfg.Updates.SilenceDurations {
		parsed, err := time.ParseDuration(d)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse silence duration %q: %w", d, err)
		}
		durations[d] = parsed
	}

	var undoTracker *server.ButtonTracker
	if cfg.Updates.UndoWindow > 0 {
		undoTracker = server.NewButtonTracker(cfg.Updates.UndoWindow)
		metrics.RegisterSizeGauge("alertly_undo_tracker_entries",
			"Current number of messages with an active Undo button.", undoTracker.Len)
	}

	handler := server.NewCallbackHandler(server.CallbackDeps{
		Logger:          logger,
		Telegram:        tgClient,
		AM:              amClient,
		Cache:           cache,
		Tracker:         tracker,
		ChatAllowlist:   cfg.Updates.ChatAllowlist,
		UserAllowlist:   cfg.Updates.UserAllowlist,
		Durations:       durations,
		SilenceMatchers: cfg.Updates.SilenceMatchers,
		UndoTracker:     undoTracker,
	})

	keyboard := &server.AlertmanagerKeyboard{
		Durations:     cfg.Updates.SilenceDurations,
		ChatAllowlist: cfg.Updates.ChatAllowlist,
		Cache:         cache,
		Logger:        logger,
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

	workers := []func(context.Context){poller.Run, sweeper.Run}
	if undoTracker != nil {
		undoSweeper := &server.ButtonSweeper{
			Tracker:  undoTracker,
			Telegram: tgClient,
			Logger:   logger,
			Interval: 30 * time.Second,
		}
		workers = append(workers, undoSweeper.Run)
	}

	logger.Info("telegram updates enabled",
		"chat_allowlist", len(cfg.Updates.ChatAllowlist),
		"user_allowlist", len(cfg.Updates.UserAllowlist),
		"durations", cfg.Updates.SilenceDurations,
		"button_ttl", cfg.Updates.ButtonTTL,
		"silence_matchers", cfg.Updates.SilenceMatchers,
		"undo_window", cfg.Updates.UndoWindow,
		"alertmanager_url", cfg.Alertmanager.URL,
	)
	return keyboard, tracker, workers, nil
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

// telegramHealthLoop drives readiness from Telegram getMe. During startup any
// failure keeps the pod unready (original startupCheck behaviour). Once ready,
// it keeps probing every probeInterval so an outage is detected even when no
// webhooks are flowing; a few consecutive failures are tolerated to avoid
// flapping on transient errors. It never overrides a send-failure-driven
// unready state with MarkReady: recovery from that path comes via
// RecordSendSuccess, probe success only re-arms probe-driven unreadiness.
func telegramHealthLoop(ctx context.Context, c telegram.Client, r server.ReadinessTracker, logger *slog.Logger, probeInterval time.Duration) {
	const (
		probeTimeout     = 10 * time.Second
		maxBackoff       = 30 * time.Second
		failureThreshold = 3
	)
	backoff := time.Second
	consecFails := 0
	everReady := false
	probeUnready := true // startup counts as probe-driven unreadiness

	for {
		callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := c.GetMe(callCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		if err == nil {
			if probeUnready {
				logger.Info("telegram getMe ok; readiness=ready")
				r.MarkReady()
				probeUnready = false
			}
			everReady = true
			consecFails = 0
			backoff = time.Second
			wait = probeInterval
		} else {
			consecFails++
			logger.Warn("telegram getMe failed",
				"err", err,
				"consecutive", consecFails,
				"next_retry_ms", backoff.Milliseconds(),
			)
			if !everReady || consecFails >= failureThreshold {
				r.MarkUnready("telegram getMe failed: " + err.Error())
				probeUnready = true
			}
			wait = backoff
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

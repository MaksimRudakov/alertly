package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/server"
	"github.com/MaksimRudakov/alertly/internal/sink"
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

	registry := metrics.Init()
	metrics.BuildInfo.WithLabelValues(version.Version, version.Commit, version.GoVersion()).Set(1)

	dryRun := config.DryRun()

	renderer, err := tmpl.New(cfg.Templates)
	if err != nil {
		return fmt.Errorf("templates: %w", err)
	}

	sinks := map[string]sink.Sink{}
	trackers := map[string]server.ReadinessTracker{}
	var tgClient telegram.Client
	if cfg.Telegram.Enabled {
		botToken := requireEnv("TELEGRAM_BOT_TOKEN")
		if botToken == "" {
			return errors.New("TELEGRAM_BOT_TOKEN is required when telegram.enabled is true")
		}
		limiter := telegram.NewLimiter(cfg.Telegram.RateLimit.GlobalPerSec, cfg.Telegram.RateLimit.PerChatPerSec)
		tgClient = telegram.New(telegram.Config{
			APIURL:         cfg.Telegram.APIURL,
			Token:          botToken,
			ParseMode:      cfg.Telegram.ParseMode,
			RequestTimeout: cfg.Telegram.RequestTimeout,
			MaxAttempts:    cfg.Telegram.Retry.MaxAttempts,
			InitialBackoff: cfg.Telegram.Retry.InitialBackoff,
			MaxBackoff:     cfg.Telegram.Retry.MaxBackoff,
			DryRun:         dryRun,
			PollMessages:   cfg.Updates.Enabled && cfg.Updates.Commands.Enabled,
		}, limiter, logger)
		sinks[sink.Telegram] = telegram.NewSink(tgClient, renderer, cfg.Format.Telegram == config.FormatBuiltin, cfg.Format.Labels)
		trackers[sink.Telegram] = server.NewReadiness()
	}
	if cfg.Slack.Enabled {
		s, err := newSlackSink(cfg, renderer, dryRun, logger)
		if err != nil {
			return fmt.Errorf("slack: %w", err)
		}
		sinks[sink.Slack] = s
		trackers[sink.Slack] = server.NewReadinessWithReason("startup: slack auth.test pending")
	}
	readiness := server.NewSinkReadiness(trackers)
	for name, tr := range trackers {
		metrics.RegisterSinkReadyGauge(name, func() bool { ok, _ := tr.IsReady(); return ok })
	}

	clusters, clusterTokens, err := buildClusters(cfg)
	if err != nil {
		return err
	}
	authToken := requireEnv(config.DefaultAuthTokenEnv)
	if authToken == "" && len(clusterTokens) == 0 {
		return errors.New("WEBHOOK_AUTH_TOKEN is required (or define clusters with auth_token_env)")
	}
	for name, tok := range clusterTokens {
		if tok == authToken {
			return fmt.Errorf("clusters.%s: its token equals WEBHOOK_AUTH_TOKEN; every cluster needs its own token", name)
		}
	}

	sources := map[string]source.Source{
		"alertmanager": source.NewAlertmanager(),
		"kubewatch":    source.NewKubewatch(),
		"generic":      source.NewGeneric(),
	}

	var dedupCache *dedup.Cache
	if cfg.Dedup.Enabled {
		dedupCache = dedup.New(cfg.Dedup.TTL)
		logger.Info("dedup enabled", "ttl", cfg.Dedup.TTL)
	}

	activity := server.NewActivityTracker()
	status := &server.StatusReporter{
		StartedAt: time.Now(),
		Version:   version.Version,
		Commit:    version.Commit,
		Readiness: trackers[sink.Telegram],
		Activity:  activity,
	}
	if dedupCache != nil {
		status.Sizes = append(status.Sizes, server.SizeStat{Label: "Dedup cache", Len: dedupCache.Len})
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
			keyboard, trackerReg, bgWorkers, err = setupUpdates(cfg, tgClient, logger, status, clusters)
			if err != nil {
				return fmt.Errorf("updates: %w", err)
			}
		}
	}

	srv := server.New(cfg.Server, server.Deps{
		Logger:                logger,
		Sources:               sources,
		Renderer:              renderer,
		Sinks:                 sinks,
		SinkReadiness:         readiness,
		AuthToken:             authToken,
		Clusters:              clusters,
		ClusterTokens:         clusterTokens,
		Registry:              registry,
		ChatAllowlist:         cfg.Telegram.ChatAllowlist,
		SlackChannelAllowlist: cfg.Slack.ChannelAllowlist,
		Keyboard:              keyboard,
		Tracker:               trackerReg,
		Dedup:                 dedupCache,
		Activity:              activity,
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
		for _, tr := range trackers {
			tr.MarkReady()
		}
		logger.Warn("DRY_RUN active: messenger calls are skipped")
	} else {
		for name, s := range sinks {
			startWorker(func(ctx context.Context) {
				sinkHealthLoop(ctx, name, s.Probe, trackers[name], logger, time.Minute)
			})
		}
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

// buildClusters turns the resolved config clusters into runtime clusters
// (targets, per-cluster Alertmanager clients) and reads each named cluster's
// webhook token from its env var.
func buildClusters(cfg config.Config) (map[string]*server.Cluster, map[string]string, error) {
	clusters := map[string]*server.Cluster{}
	tokens := map[string]string{}
	seen := map[string]string{}
	for _, rc := range cfg.ResolvedClusters() {
		c := &server.Cluster{
			Name:          rc.Name,
			Alias:         rc.Alias,
			Implicit:      rc.Implicit,
			WatchdogAlert: rc.WatchdogAlert,
			Destinations:  map[string][]sink.Target{},
		}
		for dest, specs := range rc.Destinations {
			c.Destinations[dest] = server.TargetsFromConfig(specs)
		}
		if rc.Alertmanager.URL != "" {
			prefix := rc.Alertmanager.AuthEnvPrefix
			if prefix == "" {
				prefix = config.DefaultAMAuthEnvPrefix
			}
			c.AM = alertmanager.New(alertmanager.Config{
				URL:            rc.Alertmanager.URL,
				RequestTimeout: rc.Alertmanager.RequestTimeout,
				Auth: alertmanager.Auth{
					Username: os.Getenv(prefix + "_USERNAME"),
					Password: os.Getenv(prefix + "_PASSWORD"),
					Token:    os.Getenv(prefix + "_TOKEN"),
				},
			})
		}
		if !rc.Implicit {
			tok := requireEnv(rc.AuthTokenEnv)
			if tok == "" {
				return nil, nil, fmt.Errorf("clusters.%s: env %s (auth_token_env) is empty", rc.Name, rc.AuthTokenEnv)
			}
			if other, dup := seen[tok]; dup {
				return nil, nil, fmt.Errorf("clusters.%s: token equals the one of cluster %q; every cluster needs its own token", rc.Name, other)
			}
			seen[tok] = rc.Name
			tokens[rc.Name] = tok
		}
		clusters[rc.Name] = c
	}
	return clusters, tokens, nil
}

// pipelineCluster picks the cluster whose Alertmanager /status reports on:
// the default cluster when it has one, else the first cluster (by name) that
// does. Multi-cluster /status is a later stage.
func pipelineCluster(clusters map[string]*server.Cluster) *server.Cluster {
	if c := clusters[config.DefaultCluster]; c != nil && c.AM != nil {
		return c
	}
	names := make([]string, 0, len(clusters))
	for n := range clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if clusters[n].AM != nil {
			return clusters[n]
		}
	}
	return nil
}

func setupUpdates(cfg config.Config, tgClient telegram.Client, logger *slog.Logger, status *server.StatusReporter, clusters map[string]*server.Cluster) (server.KeyboardBuilder, server.ButtonRegistrar, []func(context.Context), error) {
	cache := alertmanager.NewLabelCache(cfg.Updates.LabelCacheTTL, cfg.Updates.LabelCacheMax)
	tracker := server.NewButtonTracker(cfg.Updates.ButtonTTL, cfg.Updates.ButtonTrackerMax)
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
		undoTracker = server.NewButtonTracker(cfg.Updates.UndoWindow, cfg.Updates.ButtonTrackerMax)
		metrics.RegisterSizeGauge("alertly_undo_tracker_entries",
			"Current number of messages with an active Undo button.", undoTracker.Len)
	}

	byAlias := make(map[string]*server.Cluster, len(clusters))
	for _, c := range clusters {
		byAlias[c.Alias] = c
	}

	handler := server.NewCallbackHandler(server.CallbackDeps{
		Logger:          logger,
		Telegram:        tgClient,
		Clusters:        byAlias,
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

	status.Sizes = append(status.Sizes,
		server.SizeStat{Label: "Silence buttons", Len: tracker.Len},
		server.SizeStat{Label: "Label cache", Len: cache.Len},
	)
	if undoTracker != nil {
		status.Sizes = append(status.Sizes, server.SizeStat{Label: "Undo buttons", Len: undoTracker.Len})
	}

	var msgHandler *server.MessageHandler
	if cfg.Updates.Commands.Enabled {
		if pc := pipelineCluster(clusters); pc != nil {
			status.AM = pc.AM
			status.Pipeline = server.PipelineConfig{
				Enabled:       cfg.Updates.Commands.Status.Pipeline,
				WatchdogAlert: pc.WatchdogAlert,
				Timeout:       cfg.Updates.Commands.Status.PipelineTimeout,
			}
		}
		msgHandler = server.NewMessageHandler(server.CommandDeps{
			Logger:        logger,
			Telegram:      tgClient,
			ChatAllowlist: cfg.Updates.ChatAllowlist,
			UserAllowlist: cfg.Updates.UserAllowlist,
			Status:        status,
		})
	}

	poller := &server.UpdatesPoller{
		Client:      tgClient,
		Handler:     handler,
		Messages:    msgHandler,
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

	withAM := 0
	for _, c := range clusters {
		if c.AM != nil {
			withAM++
		}
	}
	logger.Info("telegram updates enabled",
		"chat_allowlist", len(cfg.Updates.ChatAllowlist),
		"user_allowlist", len(cfg.Updates.UserAllowlist),
		"durations", cfg.Updates.SilenceDurations,
		"button_ttl", cfg.Updates.ButtonTTL,
		"silence_matchers", cfg.Updates.SilenceMatchers,
		"undo_window", cfg.Updates.UndoWindow,
		"commands_enabled", cfg.Updates.Commands.Enabled,
		"clusters_with_alertmanager", withAM,
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

// sinkHealthLoop drives one sink's readiness from its probe (Telegram getMe,
// Slack auth.test). During startup any failure keeps the sink unready. Once
// ready, it keeps probing every probeInterval so an outage is detected even
// when no webhooks are flowing; a few consecutive failures are tolerated to
// avoid flapping on transient errors.
func sinkHealthLoop(ctx context.Context, name string, probe func(context.Context) error, r server.ReadinessTracker, logger *slog.Logger, probeInterval time.Duration) {
	const (
		probeTimeout     = 10 * time.Second
		maxBackoff       = 30 * time.Second
		failureThreshold = 3
	)
	logger = logger.With("sink", name)
	backoff := time.Second
	consecFails := 0
	everReady := false

	for {
		callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := probe(callCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		r.Touch()

		var wait time.Duration
		if err == nil {
			// A successful probe clears unreadiness whoever set it. Restricting
			// this to probe-driven unreadiness made the send-failure window
			// self-locking: RecordSendFailure flips the pod unready, the pod
			// leaves the Service endpoints, no webhook can arrive, so no send
			// can succeed — and only RecordSendSuccess would have cleared it.
			// The probe exercises the very API the failed sends went to, so its
			// success is sufficient evidence the path is usable again.
			if ready, _ := r.IsReady(); !ready {
				logger.Info("sink probe ok; readiness=ready")
				r.MarkReady()
			}
			everReady = true
			consecFails = 0
			backoff = time.Second
			wait = probeInterval
		} else {
			consecFails++
			logger.Warn("sink probe failed",
				"err", err,
				"consecutive", consecFails,
				"next_retry_ms", backoff.Milliseconds(),
			)
			if !everReady || consecFails >= failureThreshold {
				r.MarkUnready(name + " probe failed: " + err.Error())
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

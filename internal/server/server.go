package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/source"
	"github.com/MaksimRudakov/alertly/internal/telegram"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
)

// maxHeaderBytes caps request headers. Webhook clients send a bearer token
// and little else; net/http's 1 MB default is far more than needed.
const maxHeaderBytes = 64 << 10

type Server struct {
	cfg    config.Server
	logger *slog.Logger
	srv    *http.Server
}

type Deps struct {
	Logger   *slog.Logger
	Sources  map[string]source.Source
	Renderer tmpl.Renderer
	// Telegram and Readiness are the single-sink wiring: when Sinks is nil,
	// New builds a template-rendering Telegram sink from Telegram + Renderer
	// and uses Readiness as its tracker.
	Telegram  telegram.Client
	Readiness ReadinessTracker
	// Sinks are the enabled messengers keyed by sink name; SinkReadiness
	// holds one tracker per sink.
	Sinks         map[string]sink.Sink
	SinkReadiness *SinkReadiness
	// AuthToken guards the legacy /v1/{source}/{chats} routes of the default
	// cluster; empty disables them.
	AuthToken string
	// Clusters are served on /v1/clusters/{cluster}/{source}/{destination},
	// each guarded by its own token in ClusterTokens. nil = only an implicit
	// default cluster.
	Clusters      map[string]*Cluster
	ClusterTokens map[string]string
	Registry      *prometheus.Registry
	// ChatAllowlist restricts which chat IDs legacy webhook URLs may target
	// (telegram.chat_allowlist). Empty = any chat.
	ChatAllowlist []int64
	// SlackChannelAllowlist is the same for Slack channels.
	SlackChannelAllowlist []string
	Keyboard              KeyboardBuilder
	Tracker               ButtonRegistrar
	Dedup                 *dedup.Cache
	Activity              *ActivityTracker
}

func New(cfg config.Server, deps Deps) *Server {
	if deps.Sinks == nil {
		deps.Sinks = map[string]sink.Sink{sink.Telegram: telegram.NewSink(deps.Telegram, deps.Renderer, false, nil)}
	}
	if deps.SinkReadiness == nil {
		deps.SinkReadiness = NewSinkReadiness(map[string]ReadinessTracker{sink.Telegram: deps.Readiness})
	}
	if deps.Clusters == nil {
		deps.Clusters = map[string]*Cluster{config.DefaultCluster: {Name: config.DefaultCluster, Alias: config.DefaultCluster, Implicit: true}}
	}

	mux := http.NewServeMux()

	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	}))

	mux.Handle("GET /readyz", readyzHandler(deps.SinkReadiness))

	mux.Handle("GET /metrics", promhttp.HandlerFor(deps.Registry, promhttp.HandlerOpts{Registry: deps.Registry}))

	withDeadline := requestTimeoutMiddleware(cfg.WriteTimeout)
	clusterAuth := clusterAuthMiddleware(deps.ClusterTokens)

	for name, src := range deps.Sources {
		base := webhookDeps{
			source:       src,
			sinks:        deps.Sinks,
			readiness:    deps.SinkReadiness,
			maxBodyBytes: cfg.MaxBodyBytes,
			templateName: name,
			keyboard:     deps.Keyboard,
			tracker:      deps.Tracker,
			dedup:        deps.Dedup,
			activity:     deps.Activity,
		}
		if deps.AuthToken != "" {
			legacy := base
			legacy.resolve = legacyResolver(deps.Clusters[config.DefaultCluster], deps.Sinks, deps.ChatAllowlist, deps.SlackChannelAllowlist)
			mux.Handle(fmt.Sprintf("POST /v1/%s/{chats}", name), authMiddleware(deps.AuthToken)(withDeadline(webhookHandler(legacy))))
		}
		if len(deps.ClusterTokens) > 0 {
			clustered := base
			clustered.resolve = clusterResolver(deps.Clusters)
			mux.Handle(fmt.Sprintf("POST /v1/clusters/{cluster}/%s/{destination}", name), clusterAuth(withDeadline(webhookHandler(clustered))))
		}
	}

	root := chain(mux,
		recoverMiddleware,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
	)

	return &Server{
		cfg:    cfg,
		logger: deps.Logger,
		srv: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           root,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
		},
	}
}

func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http server starting", "addr", s.cfg.ListenAddr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutdown requested", "reason", ctx.Err())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("graceful shutdown failed", "err", err)
			return err
		}
		s.logger.Info("http server stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

func readyzHandler(r *SinkReadiness) http.Handler {
	type sinkBody struct {
		Ready     bool      `json:"ready"`
		Reason    string    `json:"reason,omitempty"`
		LastCheck time.Time `json:"last_check"`
	}
	type response struct {
		Ready bool `json:"ready"`
		// TelegramAPI/Reason/LastCheck keep the pre-Slack shape for the
		// telegram sink (or the first sink when Telegram is disabled).
		TelegramAPI string              `json:"telegram_api,omitempty"`
		Reason      string              `json:"reason,omitempty"`
		LastCheck   time.Time           `json:"last_check"`
		Sinks       map[string]sinkBody `json:"sinks"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ready, reason := r.IsReady()
		body := response{Ready: ready, Sinks: map[string]sinkBody{}}
		for _, st := range r.States() {
			body.Sinks[st.Name] = sinkBody{Ready: st.Ready, Reason: st.Reason, LastCheck: st.LastCheck}
			if st.Name == sink.Telegram {
				body.TelegramAPI = "ok"
				if !st.Ready {
					body.TelegramAPI = "failed"
				}
			}
			if st.LastCheck.After(body.LastCheck) {
				body.LastCheck = st.LastCheck
			}
		}
		status := http.StatusOK
		if !ready {
			body.Reason = reason
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

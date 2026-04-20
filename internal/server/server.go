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
	"github.com/MaksimRudakov/alertly/internal/source"
	tmpl "github.com/MaksimRudakov/alertly/internal/template"
	"github.com/MaksimRudakov/alertly/internal/telegram"
)

type Server struct {
	cfg       config.Server
	logger    *slog.Logger
	srv       *http.Server
	readiness ReadinessTracker
}

type Deps struct {
	Logger    *slog.Logger
	Sources   map[string]source.Source
	Renderer  tmpl.Renderer
	Telegram  telegram.Client
	Readiness ReadinessTracker
	AuthToken string
	Registry  *prometheus.Registry
}

func New(cfg config.Server, deps Deps) *Server {
	mux := http.NewServeMux()

	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	}))

	mux.Handle("GET /readyz", readyzHandler(deps.Readiness))

	mux.Handle("GET /metrics", promhttp.HandlerFor(deps.Registry, promhttp.HandlerOpts{Registry: deps.Registry}))

	auth := authMiddleware(deps.AuthToken)

	for name, src := range deps.Sources {
		h := webhookHandler(webhookDeps{
			source:       src,
			renderer:     deps.Renderer,
			tg:           deps.Telegram,
			readiness:    deps.Readiness,
			maxBodyBytes: cfg.MaxBodyBytes,
			templateName: name,
		})
		mux.Handle(fmt.Sprintf("POST /v1/%s/{chats}", name), auth(h))
	}

	root := chain(mux,
		recoverMiddleware,
		requestIDMiddleware,
		loggingMiddleware(deps.Logger),
	)

	return &Server{
		cfg:       cfg,
		logger:    deps.Logger,
		readiness: deps.Readiness,
		srv: &http.Server{
			Addr:         cfg.ListenAddr,
			Handler:      root,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
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

func readyzHandler(r ReadinessTracker) http.Handler {
	type response struct {
		TelegramAPI string    `json:"telegram_api"`
		Reason      string    `json:"reason,omitempty"`
		LastCheck   time.Time `json:"last_check"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ready, reason := r.IsReady()
		body := response{
			TelegramAPI: "ok",
			LastCheck:   r.LastCheck(),
		}
		status := http.StatusOK
		if !ready {
			body.TelegramAPI = "failed"
			body.Reason = reason
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

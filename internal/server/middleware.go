package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/MaksimRudakov/alertly/internal/metrics"
)

type ctxKey string

const (
	ctxKeyRequestID ctxKey = "request_id"
	ctxKeyLogger    ctxKey = "logger"
)

const bearerPrefix = "Bearer "

func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := contextWithRequestID(r.Context(), rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func loggingMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rid, _ := r.Context().Value(ctxKeyRequestID).(string)
			logger := base.With("request_id", rid, "path", r.URL.Path, "method", r.Method)
			ctx := contextWithLogger(r.Context(), logger)

			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r.WithContext(ctx))

			logger.Info("request",
				"status", rec.status,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger := loggerFrom(r.Context())
				logger.Error("panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// handlerWriteMargin is held back from the write timeout so the handler still
// has time to write its response after the last Telegram call returns.
const handlerWriteMargin = time.Second

// requestTimeoutMiddleware puts an explicit deadline on the request context.
// http.Server.WriteTimeout only arms a socket write deadline — it never reaches
// r.Context() — so without this the Telegram client sees a deadline-less
// context and its deadline-aware retry can never fire: alertly would keep
// retrying past the point where the caller (Alertmanager) gave up, deliver the
// message anyway, and get a duplicate from the caller's own retry.
func requestTimeoutMiddleware(writeTimeout time.Duration) func(http.Handler) http.Handler {
	if writeTimeout <= 0 {
		// Write timeout disabled: nothing to derive a request budget from.
		return func(next http.Handler) http.Handler { return next }
	}
	timeout := writeTimeout - handlerWriteMargin
	if timeout <= 0 {
		timeout = writeTimeout / 2
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func authMiddleware(token string) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, bearerPrefix) {
				metrics.AuthFailures.Inc()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := []byte(strings.TrimPrefix(h, bearerPrefix))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				metrics.AuthFailures.Inc()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

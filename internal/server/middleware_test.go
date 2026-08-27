package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MaksimRudakov/alertly/internal/config"
)

func TestRequestIDAcceptedWhenValid(t *testing.T) {
	for _, rid := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"01HZY3K9GQ4R8V5T2W7X0N6M1P", // ULID
		"trace.id_42",
	} {
		var seen string
		h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, _ = r.Context().Value(ctxKeyRequestID).(string)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Request-Id", rid)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if seen != rid {
			t.Errorf("valid id %q was replaced with %q", rid, seen)
		}
		if got := rec.Header().Get("X-Request-Id"); got != rid {
			t.Errorf("response header = %q, want %q", got, rid)
		}
	}
}

func TestRequestIDReplacedWhenInvalid(t *testing.T) {
	for name, rid := range map[string]string{
		"too long":    strings.Repeat("a", maxRequestIDLen+1),
		"space":       "abc def",
		"quote":       `ab"cd`,
		"non-ascii":   "идентификатор",
		"json inject": `x","user":"admin`,
		"escape seq":  "abc\x1b[31mred",
	} {
		var seen string
		h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen, _ = r.Context().Value(ctxKeyRequestID).(string)
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Request-Id", rid)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if seen == rid {
			t.Errorf("%s: invalid id %q was accepted", name, rid)
		}
		if seen == "" {
			t.Errorf("%s: no replacement id generated", name)
		}
		if got := rec.Header().Get("X-Request-Id"); got != seen {
			t.Errorf("%s: response header %q != context id %q", name, got, seen)
		}
	}
}

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	var seen string
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = r.Context().Value(ctxKeyRequestID).(string)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if seen == "" || len(seen) != 36 {
		t.Fatalf("want a generated UUID, got %q", seen)
	}
}

func TestServerTimeoutsWired(t *testing.T) {
	cfg := config.Default().Server
	s := New(cfg, Deps{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Readiness: NewReadiness(),
		Registry:  prometheus.NewRegistry(),
	})
	if s.srv.IdleTimeout != cfg.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", s.srv.IdleTimeout, cfg.IdleTimeout)
	}
	if s.srv.ReadHeaderTimeout != cfg.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", s.srv.ReadHeaderTimeout, cfg.ReadHeaderTimeout)
	}
	if s.srv.MaxHeaderBytes != maxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d", s.srv.MaxHeaderBytes, maxHeaderBytes)
	}
	if s.srv.IdleTimeout <= s.srv.ReadTimeout {
		t.Errorf("IdleTimeout %v should exceed ReadTimeout %v to keep webhook keep-alives warm",
			s.srv.IdleTimeout, s.srv.ReadTimeout)
	}
}

func TestMaxHeaderBytesLeavesRoomForRealClients(t *testing.T) {
	// MaxHeaderBytes lives in the listener loop, so a handler-level test cannot
	// exercise the rejection; guard the constant against being set too low for
	// realistic webhook requests (bearer token + tracing headers).
	if maxHeaderBytes < 8<<10 {
		t.Fatalf("maxHeaderBytes %d too small for realistic auth headers", maxHeaderBytes)
	}
}

func TestStatusRecorderUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	if got := sr.Unwrap(); got != http.ResponseWriter(rec) {
		t.Fatalf("Unwrap returned %T, want the wrapped writer", got)
	}
	// http.ResponseController must reach Flusher through the recorder.
	rc := http.NewResponseController(sr)
	if err := rc.Flush(); err != nil {
		t.Fatalf("ResponseController.Flush through statusRecorder: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
}

package alertmanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetAlertLabels_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("active") != "true" {
			t.Errorf("missing active filter")
		}
		_, _ = io.WriteString(w, `[{"fingerprint":"fp1","labels":{"alertname":"HighCPU","severity":"warning"}},{"fingerprint":"fp2","labels":{"alertname":"Other"}}]`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	labels, err := c.GetAlertLabels(context.Background(), "fp1")
	if err != nil {
		t.Fatal(err)
	}
	if labels["alertname"] != "HighCPU" || labels["severity"] != "warning" {
		t.Errorf("unexpected labels: %+v", labels)
	}
}

func TestGetAlertLabels_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	_, err := c.GetAlertLabels(context.Background(), "fp-missing")
	if !errors.Is(err, ErrAlertNotFound) {
		t.Errorf("expected ErrAlertNotFound, got %v", err)
	}
}

func TestGetAlertLabels_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `boom`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	_, err := c.GetAlertLabels(context.Background(), "fp")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 500 {
		t.Errorf("expected APIError(500), got %v", err)
	}
}

func TestCreateSilence_OK(t *testing.T) {
	var got SilenceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/silences" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"silenceID":"s-123"}`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	id, err := c.CreateSilence(context.Background(), SilenceRequest{
		Matchers:  []Matcher{{Name: "alertname", Value: "HighCPU", IsEqual: true}},
		StartsAt:  time.Now(),
		EndsAt:    time.Now().Add(time.Hour),
		CreatedBy: "tester",
		Comment:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "s-123" {
		t.Errorf("silence id: %s", id)
	}
	if len(got.Matchers) != 1 || got.Matchers[0].Name != "alertname" {
		t.Errorf("matchers: %+v", got.Matchers)
	}
}

func TestCreateSilence_AMRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `invalid matcher`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	_, err := c.CreateSilence(context.Background(), SilenceRequest{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 400 {
		t.Errorf("expected APIError(400), got %v", err)
	}
}

func TestAuth_Bearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, Auth: Auth{Token: "tkn"}, RequestTimeout: time.Second})
	_, _ = c.GetAlertLabels(context.Background(), "fp")
	if gotAuth != "Bearer tkn" {
		t.Errorf("auth: %s", gotAuth)
	}
}

func TestAuth_Basic(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, Auth: Auth{Username: "u", Password: "p"}, RequestTimeout: time.Second})
	_, _ = c.GetAlertLabels(context.Background(), "fp")
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("expected Basic auth, got %q", gotAuth)
	}
}

func TestMatchersFromLabels(t *testing.T) {
	m := MatchersFromLabels(map[string]string{"a": "1", "b": "2"}, nil)
	if len(m) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(m))
	}
	for _, x := range m {
		if !x.IsEqual || x.IsRegex {
			t.Errorf("matcher flags wrong: %+v", x)
		}
	}
}

func TestMatchersFromLabels_Filter(t *testing.T) {
	labels := map[string]string{"alertname": "X", "namespace": "prod", "pod": "api-1"}

	m := MatchersFromLabels(labels, []string{"alertname", "namespace"})
	if len(m) != 2 {
		t.Fatalf("expected 2 filtered matchers, got %+v", m)
	}

	// Filter entries absent from the alert are skipped, possibly down to zero.
	m = MatchersFromLabels(labels, []string{"team"})
	if len(m) != 0 {
		t.Errorf("expected 0 matchers for absent label, got %+v", m)
	}
}

func TestDeleteSilence(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	if err := c.DeleteSilence(context.Background(), "sil-42"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v2/silence/sil-42" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}

	if err := c.DeleteSilence(context.Background(), ""); err == nil {
		t.Error("empty silence id must error")
	}
}

func TestDeleteSilence_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `silence not found`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	err := c.DeleteSilence(context.Background(), "gone")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 404 {
		t.Errorf("expected APIError(404), got %v", err)
	}
}

// Transient 5xx must be retried transparently for both the labels lookup and
// silence creation; a persistent 4xx must not be retried.
func TestGetAlertLabels_RetriesTransient5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `[{"fingerprint":"fp","labels":{"alertname":"X"}}]`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	labels, err := c.GetAlertLabels(context.Background(), "fp")
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if labels["alertname"] != "X" {
		t.Errorf("labels: %#v", labels)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestCreateSilence_RetriesTransient5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		// The request body must be re-sent intact on retry.
		var req SilenceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Matchers) != 1 {
			t.Errorf("retry lost request body: err=%v matchers=%+v", err, req.Matchers)
		}
		_, _ = io.WriteString(w, `{"silenceID":"s-retry"}`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	id, err := c.CreateSilence(context.Background(), SilenceRequest{
		Matchers: []Matcher{{Name: "alertname", Value: "X", IsEqual: true}},
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if id != "s-retry" {
		t.Errorf("silence id: %s", id)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestCreateSilence_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `invalid matcher`)
	}))
	defer srv.Close()

	c := New(Config{URL: srv.URL, RequestTimeout: 2 * time.Second})
	if _, err := c.CreateSilence(context.Background(), SilenceRequest{}); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("4xx must not be retried, got %d attempts", calls.Load())
	}
}

func TestLabelCache_Len(t *testing.T) {
	c := NewLabelCache(time.Hour, 10)
	if c.Len() != 0 {
		t.Errorf("empty cache Len: %d", c.Len())
	}
	c.Put("a", map[string]string{"x": "1"})
	c.Put("b", map[string]string{"x": "2"})
	c.Put("a", map[string]string{"x": "3"}) // update, not a new entry
	if c.Len() != 2 {
		t.Errorf("Len after 2 distinct puts: %d", c.Len())
	}
	var nilCache *LabelCache
	if nilCache.Len() != 0 {
		t.Error("nil cache Len should be 0")
	}
}

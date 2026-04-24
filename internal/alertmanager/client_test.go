package alertmanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	m := MatchersFromLabels(map[string]string{"a": "1", "b": "2"})
	if len(m) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(m))
	}
	for _, x := range m {
		if !x.IsEqual || x.IsRegex {
			t.Errorf("matcher flags wrong: %+v", x)
		}
	}
}

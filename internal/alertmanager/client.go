package alertmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Config struct {
	URL            string
	RequestTimeout time.Duration
	Auth           Auth
}

type Auth struct {
	Username string
	Password string
	Token    string
}

func (a Auth) apply(req *http.Request) {
	switch {
	case a.Token != "":
		req.Header.Set("Authorization", "Bearer "+a.Token)
	case a.Username != "" || a.Password != "":
		req.SetBasicAuth(a.Username, a.Password)
	}
}

type Client interface {
	GetAlertLabels(ctx context.Context, fingerprint string) (map[string]string, error)
	CreateSilence(ctx context.Context, req SilenceRequest) (string, error)
	DeleteSilence(ctx context.Context, silenceID string) error
}

type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type SilenceRequest struct {
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
}

type silenceResponse struct {
	SilenceID string `json:"silenceID"`
}

type alert struct {
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("alertmanager API %d", e.StatusCode)
	}
	return fmt.Sprintf("alertmanager API %d: %s", e.StatusCode, e.Body)
}

var ErrAlertNotFound = errors.New("alert not found in alertmanager")

type client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) Client {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	return &client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.RequestTimeout},
	}
}

// Transient AM failures (restart, rollout, brief network blip) should not turn
// a button press into a user-visible error, so both calls retry a couple of
// times. A duplicate silence caused by "created but 5xx on response" is
// harmless: identical matchers suppress the same alerts.
const (
	retryAttempts = 3
	retryBackoff  = 300 * time.Millisecond
)

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// doWithRetry executes build() up to retryAttempts times, retrying network
// errors, 429 and 5xx with linear backoff. The request is rebuilt per attempt
// because its body reader is consumed. Returns the status code and the
// (bounded) response body of the final attempt.
func (c *client) doWithRetry(ctx context.Context, maxBody int64, build func() (*http.Request, error)) (int, []byte, error) {
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * retryBackoff):
			}
		}

		req, err := build()
		if err != nil {
			return 0, nil, err
		}
		c.cfg.Auth.apply(req)

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return 0, nil, err
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()

		if retryableStatus(resp.StatusCode) && attempt < retryAttempts-1 {
			lastErr = &APIError{StatusCode: resp.StatusCode, Body: string(body)}
			continue
		}
		return resp.StatusCode, body, nil
	}
	return 0, nil, lastErr
}

func (c *client) GetAlertLabels(ctx context.Context, fingerprint string) (map[string]string, error) {
	if fingerprint == "" {
		return nil, errors.New("fingerprint is empty")
	}
	// AM v2 API does not expose fingerprint filter directly; fetch active+silenced
	// and match client-side. Typical alert volume makes this acceptable.
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse alertmanager url: %w", err)
	}
	u.Path = joinPath(u.Path, "/api/v2/alerts")
	q := u.Query()
	q.Set("active", "true")
	q.Set("silenced", "true")
	q.Set("inhibited", "true")
	u.RawQuery = q.Encode()

	status, body, err := c.doWithRetry(ctx, 1<<20, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("alertmanager get alerts: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, &APIError{StatusCode: status, Body: string(body)}
	}

	var alerts []alert
	if err := json.Unmarshal(body, &alerts); err != nil {
		return nil, fmt.Errorf("decode alerts: %w", err)
	}
	for _, a := range alerts {
		if a.Fingerprint == fingerprint {
			return a.Labels, nil
		}
	}
	return nil, ErrAlertNotFound
}

func (c *client) CreateSilence(ctx context.Context, sreq SilenceRequest) (string, error) {
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return "", fmt.Errorf("parse alertmanager url: %w", err)
	}
	u.Path = joinPath(u.Path, "/api/v2/silences")

	body, err := json.Marshal(sreq)
	if err != nil {
		return "", fmt.Errorf("marshal silence: %w", err)
	}

	status, respBody, err := c.doWithRetry(ctx, 1<<16, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return "", fmt.Errorf("alertmanager create silence: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", &APIError{StatusCode: status, Body: string(respBody)}
	}

	var sr silenceResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return "", fmt.Errorf("decode silence response: %w", err)
	}
	if sr.SilenceID == "" {
		return "", fmt.Errorf("alertmanager returned empty silenceID: %s", string(respBody))
	}
	return sr.SilenceID, nil
}

func (c *client) DeleteSilence(ctx context.Context, silenceID string) error {
	if silenceID == "" {
		return errors.New("silence id is empty")
	}
	u, err := url.Parse(c.cfg.URL)
	if err != nil {
		return fmt.Errorf("parse alertmanager url: %w", err)
	}
	u.Path = joinPath(u.Path, "/api/v2/silence/"+url.PathEscape(silenceID))

	status, body, err := c.doWithRetry(ctx, 1<<16, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("alertmanager delete silence: %w", err)
	}
	if status < 200 || status >= 300 {
		return &APIError{StatusCode: status, Body: string(body)}
	}
	return nil
}

func joinPath(base, rel string) string {
	if base == "" {
		return rel
	}
	if base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if rel == "" {
		return base
	}
	if rel[0] != '/' {
		rel = "/" + rel
	}
	return base + rel
}

// MatchersFromLabels builds exact-match matchers from a label map.
// With an empty filter every label is included (narrowest possible silence);
// otherwise only the listed label names are used — a broader silence that also
// catches sibling alerts sharing those labels. Filtered names absent from the
// alert are skipped, so the result can be empty: callers must refuse to create
// a silence with zero matchers (it would match everything).
func MatchersFromLabels(labels map[string]string, filter []string) []Matcher {
	if len(filter) == 0 {
		out := make([]Matcher, 0, len(labels))
		for k, v := range labels {
			out = append(out, Matcher{Name: k, Value: v, IsRegex: false, IsEqual: true})
		}
		return out
	}
	out := make([]Matcher, 0, len(filter))
	for _, name := range filter {
		if v, ok := labels[name]; ok {
			out = append(out, Matcher{Name: name, Value: v, IsRegex: false, IsEqual: true})
		}
	}
	return out
}

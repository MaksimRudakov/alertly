package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MaksimRudakov/alertly/internal/alertmanager"
	"github.com/MaksimRudakov/alertly/internal/config"
	"github.com/MaksimRudakov/alertly/internal/dedup"
	"github.com/MaksimRudakov/alertly/internal/metrics"
	"github.com/MaksimRudakov/alertly/internal/notification"
	"github.com/MaksimRudakov/alertly/internal/sink"
	"github.com/MaksimRudakov/alertly/internal/source"
)

// recordingSink is an in-memory sink.Sink.
type recordingSink struct {
	name  string
	delay time.Duration
	fail  error

	mu     sync.Mutex
	sent   []sinkSend
	nextID int
}

type sinkSend struct {
	Target  sink.Target
	Text    string
	Cluster string
	Actions *sink.Actions
}

func (s *recordingSink) Name() string { return s.name }

func (s *recordingSink) Render(_ string, n notification.Notification) ([]sink.Part, error) {
	return []sink.Part{{Text: n.Cluster + "/" + n.Title}}, nil
}

func (s *recordingSink) Send(ctx context.Context, t sink.Target, p sink.Part, a *sink.Actions) (sink.MessageRef, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return sink.MessageRef{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cluster, _, _ := strings.Cut(p.Text, "/")
	s.sent = append(s.sent, sinkSend{Target: t, Text: p.Text, Cluster: cluster, Actions: a})
	if s.fail != nil {
		return sink.MessageRef{}, s.fail
	}
	s.nextID++
	return sink.MessageRef{Sink: s.name, Chat: t.Chat, ID: strconv.Itoa(s.nextID)}, nil
}

func (s *recordingSink) Probe(context.Context) error { return nil }

func (s *recordingSink) Classify(err error) sink.ErrorClass {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sink.ErrCanceled
	}
	return sink.ErrServer
}

func (s *recordingSink) Sent() []sinkSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkSend(nil), s.sent...)
}

type clusterEnv struct {
	ts       *httptest.Server
	tg       *recordingSink
	slack    *recordingSink
	tgReady  ReadinessTracker
	slReady  ReadinessTracker
	clusters map[string]*Cluster
}

func newClusterEnv(t *testing.T, cache *dedup.Cache) *clusterEnv {
	t.Helper()
	metrics.Init()
	e := &clusterEnv{
		tg:      &recordingSink{name: sink.Telegram},
		slack:   &recordingSink{name: sink.Slack},
		tgReady: NewReadiness(),
		slReady: NewReadinessWithReason("startup"),
	}
	e.tgReady.MarkReady()
	e.slReady.MarkReady()
	e.clusters = map[string]*Cluster{
		config.DefaultCluster: {Name: config.DefaultCluster, Alias: config.DefaultCluster, Implicit: true},
		"k8s-prod": {Name: "k8s-prod", Alias: "prod", Destinations: map[string][]sink.Target{
			"default": {{Sink: sink.Telegram, Chat: "-100"}},
		}},
		"k8s-data": {Name: "k8s-data", Alias: "data", Destinations: map[string][]sink.Target{
			"default": {{Sink: sink.Telegram, Chat: "-100"}, {Sink: sink.Slack, Chat: "C0DATA0001"}},
		}},
	}
	s := New(config.Default().Server, Deps{
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sources:               map[string]source.Source{"alertmanager": source.NewAlertmanager(), "generic": source.NewGeneric()},
		Sinks:                 map[string]sink.Sink{sink.Telegram: e.tg, sink.Slack: e.slack},
		SinkReadiness:         NewSinkReadiness(map[string]ReadinessTracker{sink.Telegram: e.tgReady, sink.Slack: e.slReady}),
		AuthToken:             authToken,
		Clusters:              e.clusters,
		ClusterTokens:         map[string]string{"k8s-prod": "tok-prod", "k8s-data": "tok-data"},
		Registry:              prometheus.NewRegistry(),
		SlackChannelAllowlist: []string{"C0DATA0001"},
		Dedup:                 cache,
	})
	e.ts = httptest.NewServer(s.srv.Handler)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *clusterEnv) post(t *testing.T, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const oneAlert = `{"alerts":[{"status":"firing","fingerprint":"fp1","labels":{"alertname":"X"},"annotations":{"summary":"Disk full"}}]}`

func TestClusterRoute_AuthIsPerCluster(t *testing.T) {
	e := newClusterEnv(t, nil)
	cases := []struct {
		name, path, token string
		want              int
	}{
		{"own token", "/v1/clusters/k8s-prod/alertmanager/default", "tok-prod", http.StatusOK},
		{"other cluster token", "/v1/clusters/k8s-prod/alertmanager/default", "tok-data", http.StatusUnauthorized},
		{"legacy token on cluster route", "/v1/clusters/k8s-prod/alertmanager/default", authToken, http.StatusUnauthorized},
		{"unknown cluster looks like bad token", "/v1/clusters/nope/alertmanager/default", "tok-prod", http.StatusUnauthorized},
		{"unknown destination", "/v1/clusters/k8s-prod/alertmanager/missing", "tok-prod", http.StatusNotFound},
		{"cluster token on legacy route", "/v1/alertmanager/-100", "tok-prod", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := e.post(t, c.path, c.token, oneAlert)
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, c.want, body)
			}
		})
	}
	if got := len(e.tg.Sent()); got != 1 {
		t.Errorf("only the authorised request may deliver, got %d sends", got)
	}
}

func TestClusterRoute_DeliversWithClusterName(t *testing.T) {
	e := newClusterEnv(t, nil)
	resp, body := e.post(t, "/v1/clusters/k8s-data/alertmanager/default", "tok-data", oneAlert)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	tg, sl := e.tg.Sent(), e.slack.Sent()
	if len(tg) != 1 || len(sl) != 1 {
		t.Fatalf("want one telegram and one slack send, got %d/%d", len(tg), len(sl))
	}
	if tg[0].Cluster != "k8s-data" || sl[0].Target.Chat != "C0DATA0001" {
		t.Errorf("sends: %+v %+v", tg[0], sl[0])
	}
	var parsed struct {
		Attempts int                  `json:"attempts"`
		Sinks    map[string]sinkStats `json:"sinks"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Attempts != 2 || parsed.Sinks["slack"].Attempts != 1 {
		t.Errorf("response body: %s (%v)", body, err)
	}

	// Legacy route: the implicit default cluster renders no cluster name.
	resp, _ = e.post(t, "/v1/alertmanager/-100", authToken, oneAlert)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy status %d", resp.StatusCode)
	}
	if got := e.tg.Sent()[1].Cluster; got != "" {
		t.Errorf("implicit default cluster must not be named, got %q", got)
	}
}

// Alertmanager fingerprints are label hashes: without a `cluster` external
// label the same alert in two clusters shares one. Dedup must keep them apart.
func TestClusterRoute_DedupIsPerCluster(t *testing.T) {
	e := newClusterEnv(t, dedup.New(time.Hour))
	e.post(t, "/v1/clusters/k8s-prod/alertmanager/default", "tok-prod", oneAlert)
	e.post(t, "/v1/clusters/k8s-data/alertmanager/default", "tok-data", oneAlert)
	e.post(t, "/v1/clusters/k8s-prod/alertmanager/default", "tok-prod", oneAlert) // retry → deduped

	var prod, data int
	for _, s := range e.tg.Sent() {
		switch s.Cluster {
		case "k8s-prod":
			prod++
		case "k8s-data":
			data++
		}
	}
	if prod != 1 || data != 1 {
		t.Errorf("want one delivery per cluster to the shared chat, got prod=%d data=%d", prod, data)
	}
}

// One failing messenger must neither block the other nor take the pod out of
// the Service: the webhook is a partial success, readiness stays up.
func TestClusterRoute_SinkFailureIsIsolated(t *testing.T) {
	e := newClusterEnv(t, nil)
	e.slack.fail = errors.New("slack 503")
	e.slack.delay = 50 * time.Millisecond

	for i := 0; i < readyzFailureWindow; i++ {
		body := strings.Replace(oneAlert, "fp1", "fp-"+strconv.Itoa(i), 1)
		resp, _ := e.post(t, "/v1/clusters/k8s-data/alertmanager/default", "tok-data", body)
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("send %d: status %d, want 207", i, resp.StatusCode)
		}
	}
	if ok, _ := e.slReady.IsReady(); ok {
		t.Error("slack sink must be unready after the failure window")
	}
	if ok, _ := e.tgReady.IsReady(); !ok {
		t.Error("telegram sink must stay ready")
	}
	resp, err := http.Get(e.ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"slack":{"ready":false`) {
		t.Errorf("readyz: %d %s", resp.StatusCode, b)
	}
}

func TestLegacyRoute_SlackTargets(t *testing.T) {
	e := newClusterEnv(t, nil)
	cases := []struct {
		chats string
		want  int
	}{
		{"-100,slack:C0DATA0001", http.StatusOK},
		{"tg:-100:7", http.StatusOK},
		{"slack:C0OTHER001", http.StatusForbidden},
		{"slack:alerts", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, body := e.post(t, "/v1/alertmanager/"+c.chats, authToken, oneAlert)
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.chats, resp.StatusCode, c.want, body)
		}
	}
	var sawThread bool
	for _, s := range e.tg.Sent() {
		if s.Target.Thread == "7" {
			sawThread = true
		}
	}
	if !sawThread || len(e.slack.Sent()) != 1 {
		t.Errorf("tg sends %+v, slack sends %d", e.tg.Sent(), len(e.slack.Sent()))
	}
}

// Silence buttons of a named cluster carry its alias and silence in its own
// Alertmanager; a button forged for another cluster is refused.
func TestCallback_NamedClusterUsesItsAlertmanager(t *testing.T) {
	prodAM := &fakeAM{labels: map[string]map[string]string{"fp1": {"alertname": "X"}}}
	dataAM := &fakeAM{labels: map[string]map[string]string{"fp1": {"alertname": "X"}}}
	clusters := map[string]*Cluster{
		"prod": {Name: "k8s-prod", Alias: "prod", AM: prodAM},
		"data": {Name: "k8s-data", Alias: "data", AM: dataAM},
	}
	tracker := NewButtonTracker(time.Hour, 0)
	tracker.RegisterFor(-100, 42, "k8s-prod", "fp1")
	tg := &fakeTG{}
	h := NewCallbackHandler(CallbackDeps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telegram:      tg,
		Clusters:      clusters,
		Cache:         alertmanager.NewLabelCache(time.Hour, 10),
		Tracker:       tracker,
		ChatAllowlist: []int64{-100},
		Durations:     map[string]time.Duration{"1h": time.Hour},
	})

	h.Handle(context.Background(), mkCallback("s|data|fp1|1h", -100, 1))
	if len(dataAM.createdReqs)+len(prodAM.createdReqs) != 0 {
		t.Fatal("button naming another cluster than the tracked one must be refused")
	}

	h.Handle(context.Background(), mkCallback("s|prod|fp1|1h", -100, 1))
	if len(prodAM.createdReqs) != 1 || len(dataAM.createdReqs) != 0 {
		t.Fatalf("silence must go to the prod AM only: prod=%d data=%d", len(prodAM.createdReqs), len(dataAM.createdReqs))
	}
	if c := prodAM.createdReqs[0].Comment; !strings.Contains(c, "cluster k8s-prod") {
		t.Errorf("silence comment should name the cluster: %q", c)
	}
	m := tg.editedMarkups[len(tg.editedMarkups)-1].Markup
	if m != nil {
		t.Errorf("undo disabled: keyboard must be stripped, got %+v", m)
	}

	h.Handle(context.Background(), mkCallback("s|nope|fp1|1h", -100, 1))
	if last := tg.answers[len(tg.answers)-1]; !strings.Contains(last.Text, "Unknown cluster") {
		t.Errorf("unknown alias answer: %+v", last)
	}
}

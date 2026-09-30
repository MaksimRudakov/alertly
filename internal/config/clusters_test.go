package config

import (
	"strings"
	"testing"
	"time"
)

func TestResolvedClusters_ImplicitDefault(t *testing.T) {
	cfg := Default()
	cfg.Alertmanager.URL = "http://am:9093"

	got := cfg.ResolvedClusters()
	if len(got) != 1 {
		t.Fatalf("want 1 implicit cluster, got %d", len(got))
	}
	d := got[0]
	if d.Name != DefaultCluster || !d.Implicit || d.AuthTokenEnv != DefaultAuthTokenEnv {
		t.Errorf("implicit default: %+v", d)
	}
	if d.Alertmanager.URL != "http://am:9093" || d.Alertmanager.AuthEnvPrefix != DefaultAMAuthEnvPrefix {
		t.Errorf("implicit default AM: %+v", d.Alertmanager)
	}
	if d.WatchdogAlert != "Watchdog" {
		t.Errorf("watchdog: %q", d.WatchdogAlert)
	}
}

func TestLoad_Clusters(t *testing.T) {
	p := writeFile(t, `
slack:
  enabled: true
clusters:
  k8s-prod:
    alias: prod
    auth_token_env: WEBHOOK_AUTH_TOKEN_K8S_PROD
    alertmanager:
      url: http://am-prod:9093
      auth_env_prefix: ALERTMANAGER_K8S_PROD
    watchdog_alert: ""
    destinations:
      default:
        - telegram: "-1001111111111:42"
  sdlc:
    auth_token_env: WEBHOOK_AUTH_TOKEN_SDLC
    destinations:
      default:
        - telegram: "-1002222222222"
        - slack: "C0SDLCALRT"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := cfg.ResolvedClusters()
	if len(got) != 3 {
		t.Fatalf("want default + 2 clusters, got %d: %+v", len(got), got)
	}
	byName := map[string]ResolvedCluster{}
	for _, rc := range got {
		byName[rc.Name] = rc
	}
	prod := byName["k8s-prod"]
	if prod.Alias != "prod" || prod.WatchdogAlert != "" || prod.Alertmanager.RequestTimeout != 10*time.Second {
		t.Errorf("k8s-prod: %+v", prod)
	}
	sdlc := byName["sdlc"]
	if sdlc.Alias != "sdlc" || sdlc.WatchdogAlert != "Watchdog" {
		t.Errorf("sdlc alias/watchdog defaults: %+v", sdlc)
	}
	if s, a := sdlc.Destinations["default"][1].Sink(); s != SinkSlack || a != "C0SDLCALRT" {
		t.Errorf("slack target: %s %s", s, a)
	}
	if !byName[DefaultCluster].Implicit {
		t.Error("implicit default must be kept next to named clusters")
	}
	if cfg.Format.Telegram != FormatTemplate || cfg.Format.Slack != FormatBuiltin {
		t.Errorf("format defaults: %+v", cfg.Format)
	}
}

func TestValidate_Clusters(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Slack.Enabled = true
		c.Clusters = map[string]Cluster{
			"k8s-prod": {
				Alias:        "prod",
				AuthTokenEnv: "TOKEN_PROD",
				Destinations: map[string][]TargetSpec{"default": {{Telegram: "-100"}}},
			},
		}
		return c
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"valid", func(*Config) {}, ""},
		{"bad cluster name", func(c *Config) {
			c.Clusters["Prod"] = Cluster{Alias: "p2", AuthTokenEnv: "T2"}
		}, "must match"},
		{"long name without alias", func(c *Config) {
			c.Clusters["very-long-name"] = Cluster{AuthTokenEnv: "T2"}
		}, "set `alias`"},
		{"duplicate alias", func(c *Config) {
			c.Clusters["prod2"] = Cluster{Alias: "prod", AuthTokenEnv: "T2"}
		}, "already used"},
		{"alias collides with implicit default", func(c *Config) {
			c.Clusters["other"] = Cluster{Alias: "default", AuthTokenEnv: "T2"}
		}, "already used"},
		{"missing token env", func(c *Config) {
			cl := c.Clusters["k8s-prod"]
			cl.AuthTokenEnv = ""
			c.Clusters["k8s-prod"] = cl
		}, "auth_token_env"},
		{"shared token env", func(c *Config) {
			c.Clusters["sdlc"] = Cluster{AuthTokenEnv: "TOKEN_PROD"}
		}, "own token"},
		{"target with both sinks", func(c *Config) {
			c.Clusters["k8s-prod"].Destinations["x"] = []TargetSpec{{Telegram: "-1", Slack: "C12345678"}}
		}, "exactly one"},
		{"empty destination", func(c *Config) {
			c.Clusters["k8s-prod"].Destinations["x"] = nil
		}, "at least one target"},
		{"slack channel name instead of id", func(c *Config) {
			c.Clusters["k8s-prod"].Destinations["x"] = []TargetSpec{{Slack: "#alerts"}}
		}, "channel ID"},
		{"slack target with slack disabled", func(c *Config) {
			c.Slack.Enabled = false
			c.Clusters["k8s-prod"].Destinations["x"] = []TargetSpec{{Slack: "C12345678"}}
		}, "slack.enabled is false"},
		{"telegram chat outside allowlist", func(c *Config) {
			c.Telegram.ChatAllowlist = []int64{-200}
		}, "chat_allowlist"},
		{"bad telegram thread", func(c *Config) {
			c.Clusters["k8s-prod"].Destinations["x"] = []TargetSpec{{Telegram: "-100:abc"}}
		}, "thread id"},
		{"no sinks", func(c *Config) {
			c.Telegram.Enabled = false
			c.Slack.Enabled = false
			c.Clusters = nil
		}, "at least one of"},
		{"bad format", func(c *Config) { c.Format.Slack = "markdown" }, "format.slack"},
		{"bad slack allowlist", func(c *Config) { c.Slack.ChannelAllowlist = []string{"alerts"} }, "channel ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidate_UpdatesWithClusterAM(t *testing.T) {
	c := Default()
	c.Updates.Enabled = true
	c.Clusters = map[string]Cluster{
		"prod": {AuthTokenEnv: "T", Alertmanager: ClusterAlertmanager{URL: "http://am:9093"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a cluster-level AM url must satisfy updates.enabled: %v", err)
	}
	c.Clusters = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "alertmanager.url") {
		t.Fatalf("want alertmanager.url error, got %v", err)
	}
}

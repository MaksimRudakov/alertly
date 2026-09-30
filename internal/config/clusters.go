package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sink names used in destinations and target specs.
const (
	SinkTelegram = "telegram"
	SinkSlack    = "slack"
)

// DefaultCluster is the cluster served by the legacy /v1/{source}/{chats}
// route. It exists implicitly (built from the top-level alertmanager block
// and WEBHOOK_AUTH_TOKEN) unless `clusters` defines it explicitly.
const DefaultCluster = "default"

// DefaultAuthTokenEnv is the env var holding the legacy webhook token.
const DefaultAuthTokenEnv = "WEBHOOK_AUTH_TOKEN"

// MaxClusterAlias bounds the alias carried in Telegram callback_data (64-byte
// limit shared with the fingerprint and duration).
const MaxClusterAlias = 8

// Slack configures the Slack sink. Tokens come from env: SLACK_BOT_TOKEN
// (xoxb-, chat.postMessage) and, for interactivity, SLACK_APP_TOKEN (xapp-).
type Slack struct {
	Enabled        bool           `yaml:"enabled"`
	APIURL         string         `yaml:"api_url"`
	RequestTimeout time.Duration  `yaml:"request_timeout"`
	RateLimit      SlackRateLimit `yaml:"rate_limit"`
	Retry          Retry          `yaml:"retry"`
	// ChannelAllowlist limits which channel IDs webhook targets may name.
	// Empty = any channel.
	ChannelAllowlist []string `yaml:"channel_allowlist"`
}

type SlackRateLimit struct {
	PerChannelPerSec float64 `yaml:"per_channel_per_sec"`
	GlobalPerSec     float64 `yaml:"global_per_sec"`
}

// Format selects how notifications are rendered per sink: "builtin" is
// alertly's own layout (identical structure in every messenger), "template"
// uses the operator-supplied text/template entries.
type Format struct {
	Telegram string `yaml:"telegram"`
	Slack    string `yaml:"slack"`
	// Labels are shown as key fields by the builtin renderers.
	Labels []string `yaml:"labels"`
}

const (
	FormatBuiltin  = "builtin"
	FormatTemplate = "template"
)

// Cluster is one alert source with its own Alertmanager, webhook token and
// named destinations.
type Cluster struct {
	// Alias goes into Telegram callback_data; defaults to the cluster name
	// when that fits MaxClusterAlias.
	Alias        string              `yaml:"alias"`
	AuthTokenEnv string              `yaml:"auth_token_env"`
	Alertmanager ClusterAlertmanager `yaml:"alertmanager"`
	// WatchdogAlert overrides updates.commands.status.watchdog_alert for this
	// cluster; nil keeps the global value.
	WatchdogAlert *string                 `yaml:"watchdog_alert"`
	Destinations  map[string][]TargetSpec `yaml:"destinations"`
}

type ClusterAlertmanager struct {
	URL            string        `yaml:"url"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// AuthEnvPrefix names the env vars with AM credentials:
	// <prefix>_USERNAME + <prefix>_PASSWORD (basic) or <prefix>_TOKEN (bearer).
	AuthEnvPrefix string `yaml:"auth_env_prefix"`
}

// TargetSpec is one entry of a destination: exactly one sink key is set.
type TargetSpec struct {
	Telegram string `yaml:"telegram"`
	Slack    string `yaml:"slack"`
}

// Sink returns the sink name and its address.
func (t TargetSpec) Sink() (sink, address string) {
	if t.Telegram != "" {
		return SinkTelegram, t.Telegram
	}
	return SinkSlack, t.Slack
}

// ResolvedCluster is a cluster with defaults applied, ready for wiring.
type ResolvedCluster struct {
	Name          string
	Alias         string
	AuthTokenEnv  string
	Alertmanager  ClusterAlertmanager
	WatchdogAlert string
	Destinations  map[string][]TargetSpec
	// Implicit marks the default cluster synthesised from top-level config.
	Implicit bool
}

// DefaultAMAuthEnvPrefix is where the top-level Alertmanager credentials live.
const DefaultAMAuthEnvPrefix = "ALERTMANAGER_AUTH"

// ResolvedClusters returns every cluster sorted by name, including the
// implicit `default` one unless `clusters` defines it.
func (c Config) ResolvedClusters() []ResolvedCluster {
	out := make([]ResolvedCluster, 0, len(c.Clusters)+1)
	for name, cl := range c.Clusters {
		rc := ResolvedCluster{
			Name:          name,
			Alias:         cl.Alias,
			AuthTokenEnv:  cl.AuthTokenEnv,
			Alertmanager:  cl.Alertmanager,
			WatchdogAlert: c.Updates.Commands.Status.WatchdogAlert,
			Destinations:  cl.Destinations,
		}
		if rc.Alias == "" {
			rc.Alias = name
		}
		if rc.Alertmanager.RequestTimeout <= 0 {
			rc.Alertmanager.RequestTimeout = c.Alertmanager.RequestTimeout
		}
		if cl.WatchdogAlert != nil {
			rc.WatchdogAlert = *cl.WatchdogAlert
		}
		out = append(out, rc)
	}
	if _, ok := c.Clusters[DefaultCluster]; !ok {
		out = append(out, ResolvedCluster{
			Name:         DefaultCluster,
			Alias:        DefaultCluster,
			AuthTokenEnv: DefaultAuthTokenEnv,
			Alertmanager: ClusterAlertmanager{
				URL:            c.Alertmanager.URL,
				RequestTimeout: c.Alertmanager.RequestTimeout,
				AuthEnvPrefix:  DefaultAMAuthEnvPrefix,
			},
			WatchdogAlert: c.Updates.Commands.Status.WatchdogAlert,
			Implicit:      true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HasInteractiveAM reports whether any cluster can serve silence buttons.
func (c Config) HasInteractiveAM() bool {
	for _, rc := range c.ResolvedClusters() {
		if rc.Alertmanager.URL != "" {
			return true
		}
	}
	return false
}

var (
	nameRE         = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	aliasRE        = regexp.MustCompile(`^[a-z0-9-]{1,8}$`)
	slackChannelRE = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)
	envNameRE      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

func (c Config) validateSinks() error {
	if !c.Telegram.Enabled && !c.Slack.Enabled {
		return errors.New("at least one of telegram.enabled and slack.enabled must be true")
	}
	if c.Slack.Enabled {
		if c.Slack.APIURL == "" {
			return errors.New("slack.api_url is required when slack.enabled is true")
		}
		if c.Slack.RequestTimeout <= 0 {
			return errors.New("slack.request_timeout must be > 0")
		}
		if c.Slack.RateLimit.PerChannelPerSec <= 0 || c.Slack.RateLimit.GlobalPerSec <= 0 {
			return errors.New("slack.rate_limit values must be > 0")
		}
		if c.Slack.Retry.MaxAttempts <= 0 || c.Slack.Retry.InitialBackoff <= 0 ||
			c.Slack.Retry.MaxBackoff < c.Slack.Retry.InitialBackoff {
			return errors.New("slack.retry: max_attempts and initial_backoff must be > 0, max_backoff >= initial_backoff")
		}
	}
	for _, ch := range c.Slack.ChannelAllowlist {
		if !slackChannelRE.MatchString(ch) {
			return fmt.Errorf("slack.channel_allowlist: %q is not a Slack channel ID (C…/G…/D…); channel names break silently on rename", ch)
		}
	}
	switch c.Format.Telegram {
	case FormatBuiltin, FormatTemplate:
	default:
		return fmt.Errorf("format.telegram must be %q or %q, got %q", FormatTemplate, FormatBuiltin, c.Format.Telegram)
	}
	switch c.Format.Slack {
	case FormatBuiltin:
	case FormatTemplate:
		if _, ok := c.Templates["slack.default"]; c.Slack.Enabled && !ok {
			return errors.New(`format.slack: template requires a "slack.default" entry in templates (Slack mrkdwn; "slack.<source>" entries override it)`)
		}
	default:
		return fmt.Errorf("format.slack must be %q or %q, got %q", FormatBuiltin, FormatTemplate, c.Format.Slack)
	}
	return nil
}

func (c Config) validateClusters() error {
	aliases := map[string]string{}
	tokenEnvs := map[string]string{}
	for _, rc := range c.ResolvedClusters() {
		if !nameRE.MatchString(rc.Name) {
			return fmt.Errorf("clusters: name %q must match [a-z0-9-]{1,63}", rc.Name)
		}
		if !aliasRE.MatchString(rc.Alias) {
			return fmt.Errorf("clusters.%s: alias %q must match [a-z0-9-]{1,%d} (set `alias` for long cluster names)", rc.Name, rc.Alias, MaxClusterAlias)
		}
		if other, dup := aliases[rc.Alias]; dup {
			return fmt.Errorf("clusters.%s: alias %q already used by cluster %q", rc.Name, rc.Alias, other)
		}
		aliases[rc.Alias] = rc.Name
		if rc.Implicit {
			continue
		}
		if !envNameRE.MatchString(rc.AuthTokenEnv) {
			return fmt.Errorf("clusters.%s: auth_token_env %q must be an env var name", rc.Name, rc.AuthTokenEnv)
		}
		if other, dup := tokenEnvs[rc.AuthTokenEnv]; dup {
			return fmt.Errorf("clusters.%s: auth_token_env %q already used by cluster %q; each cluster needs its own token", rc.Name, rc.AuthTokenEnv, other)
		}
		tokenEnvs[rc.AuthTokenEnv] = rc.Name
		if p := rc.Alertmanager.AuthEnvPrefix; p != "" && !envNameRE.MatchString(p) {
			return fmt.Errorf("clusters.%s: alertmanager.auth_env_prefix %q must be an env var name prefix", rc.Name, p)
		}
		if rc.Alertmanager.URL != "" && rc.Alertmanager.RequestTimeout <= 0 {
			return fmt.Errorf("clusters.%s: alertmanager.request_timeout must be > 0", rc.Name)
		}
		for dest, targets := range rc.Destinations {
			if !nameRE.MatchString(dest) {
				return fmt.Errorf("clusters.%s.destinations: name %q must match [a-z0-9-]{1,63}", rc.Name, dest)
			}
			if len(targets) == 0 {
				return fmt.Errorf("clusters.%s.destinations.%s: at least one target is required", rc.Name, dest)
			}
			for i, t := range targets {
				if err := c.validateTarget(t); err != nil {
					return fmt.Errorf("clusters.%s.destinations.%s[%d]: %w", rc.Name, dest, i, err)
				}
			}
		}
	}
	return nil
}

func (c Config) validateTarget(t TargetSpec) error {
	if (t.Telegram == "") == (t.Slack == "") {
		return errors.New("exactly one of `telegram` and `slack` must be set")
	}
	if t.Telegram != "" {
		if !c.Telegram.Enabled {
			return errors.New("telegram target but telegram.enabled is false")
		}
		chat, err := ParseTelegramAddress(t.Telegram)
		if err != nil {
			return err
		}
		if len(c.Telegram.ChatAllowlist) > 0 && !int64In(chat, c.Telegram.ChatAllowlist) {
			return fmt.Errorf("telegram chat %d is not in telegram.chat_allowlist", chat)
		}
		return nil
	}
	if !c.Slack.Enabled {
		return errors.New("slack target but slack.enabled is false")
	}
	channel, _, _ := strings.Cut(t.Slack, ":")
	if !slackChannelRE.MatchString(channel) {
		return fmt.Errorf("slack target %q: channel must be a Slack channel ID (C…/G…/D…), not a name", t.Slack)
	}
	if len(c.Slack.ChannelAllowlist) > 0 && !stringIn(channel, c.Slack.ChannelAllowlist) {
		return fmt.Errorf("slack channel %s is not in slack.channel_allowlist", channel)
	}
	return nil
}

// ParseTelegramAddress validates "chat_id[:thread_id]" and returns the chat.
func ParseTelegramAddress(s string) (int64, error) {
	chatStr, threadStr, hasThread := strings.Cut(s, ":")
	chat, err := strconv.ParseInt(chatStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid telegram chat id %q", chatStr)
	}
	if hasThread {
		if _, err := strconv.Atoi(threadStr); err != nil {
			return 0, fmt.Errorf("invalid telegram thread id %q", threadStr)
		}
	}
	return chat, nil
}

func int64In(v int64, set []int64) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

func stringIn(v string, set []string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

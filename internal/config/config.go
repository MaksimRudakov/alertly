package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultPath     = "/etc/alertly/config.yaml"
	defaultTemplate = `{{ severity_emoji .Severity }} <b>{{ escape_html .Title }}</b>
{{ if .Body }}{{ escape_html .Body }}{{ end }}`
)

type Config struct {
	Server       Server            `yaml:"server"`
	Telegram     Telegram          `yaml:"telegram"`
	Templates    map[string]string `yaml:"templates"`
	Logging      Logging           `yaml:"logging"`
	Updates      Updates           `yaml:"updates"`
	Alertmanager Alertmanager      `yaml:"alertmanager"`
}

type Server struct {
	ListenAddr      string        `yaml:"listen_addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
}

type Telegram struct {
	APIURL         string        `yaml:"api_url"`
	ParseMode      string        `yaml:"parse_mode"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	RateLimit      RateLimit     `yaml:"rate_limit"`
	Retry          Retry         `yaml:"retry"`
}

type RateLimit struct {
	PerChatPerSec float64 `yaml:"per_chat_per_sec"`
	GlobalPerSec  float64 `yaml:"global_per_sec"`
}

type Retry struct {
	MaxAttempts    int           `yaml:"max_attempts"`
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

type Updates struct {
	Enabled          bool          `yaml:"enabled"`
	PollTimeout      time.Duration `yaml:"poll_timeout"`
	ChatAllowlist    []int64       `yaml:"chat_allowlist"`
	UserAllowlist    []int64       `yaml:"user_allowlist"`
	SilenceDurations []string      `yaml:"silence_durations"`
	LabelCacheTTL    time.Duration `yaml:"label_cache_ttl"`
	LabelCacheMax    int           `yaml:"label_cache_max"`
	// ButtonTTL defines the window during which a user can press the silence
	// buttons on an alert message. After this window the sweeper removes the
	// inline keyboard and late clicks are rejected. Allowed range: 2h..48h.
	ButtonTTL time.Duration `yaml:"button_ttl"`
}

const (
	ButtonTTLMin = 2 * time.Hour
	ButtonTTLMax = 48 * time.Hour
)

type Alertmanager struct {
	URL            string        `yaml:"url"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

func Default() Config {
	return Config{
		Server: Server{
			ListenAddr:      ":8080",
			ReadTimeout:     10 * time.Second,
			WriteTimeout:    30 * time.Second,
			ShutdownTimeout: 30 * time.Second,
			MaxBodyBytes:    1 << 20,
		},
		Telegram: Telegram{
			APIURL:         "https://api.telegram.org",
			ParseMode:      "HTML",
			RequestTimeout: 10 * time.Second,
			RateLimit: RateLimit{
				PerChatPerSec: 1,
				GlobalPerSec:  30,
			},
			Retry: Retry{
				MaxAttempts:    5,
				InitialBackoff: time.Second,
				MaxBackoff:     60 * time.Second,
			},
		},
		Templates: map[string]string{
			"default": defaultTemplate,
		},
		Logging: Logging{
			Level:  "info",
			Format: "json",
		},
		Updates: Updates{
			Enabled:          false,
			PollTimeout:      30 * time.Second,
			ChatAllowlist:    nil,
			UserAllowlist:    nil,
			SilenceDurations: []string{"1h", "4h", "24h"},
			LabelCacheTTL:    48 * time.Hour,
			LabelCacheMax:    10000,
			ButtonTTL:        8 * time.Hour,
		},
		Alertmanager: Alertmanager{
			URL:            "",
			RequestTimeout: 10 * time.Second,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path) // #nosec G304 -- config path is operator-supplied by design
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}

	if cfg.Templates == nil {
		cfg.Templates = map[string]string{}
	}
	if _, ok := cfg.Templates["default"]; !ok {
		cfg.Templates["default"] = defaultTemplate
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Server.ListenAddr == "" {
		return errors.New("server.listen_addr is required")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return errors.New("server.max_body_bytes must be > 0")
	}
	if c.Telegram.APIURL == "" {
		return errors.New("telegram.api_url is required")
	}
	if c.Telegram.RequestTimeout <= 0 {
		return errors.New("telegram.request_timeout must be > 0")
	}
	if c.Telegram.RateLimit.PerChatPerSec <= 0 {
		return errors.New("telegram.rate_limit.per_chat_per_sec must be > 0")
	}
	if c.Telegram.RateLimit.GlobalPerSec <= 0 {
		return errors.New("telegram.rate_limit.global_per_sec must be > 0")
	}
	if c.Telegram.Retry.MaxAttempts <= 0 {
		return errors.New("telegram.retry.max_attempts must be > 0")
	}
	if c.Telegram.Retry.InitialBackoff <= 0 {
		return errors.New("telegram.retry.initial_backoff must be > 0")
	}
	if c.Telegram.Retry.MaxBackoff < c.Telegram.Retry.InitialBackoff {
		return errors.New("telegram.retry.max_backoff must be >= initial_backoff")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid logging.level %q", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		return fmt.Errorf("invalid logging.format %q", c.Logging.Format)
	}
	if c.Updates.Enabled {
		if c.Updates.PollTimeout <= 0 {
			return errors.New("updates.poll_timeout must be > 0 when updates.enabled is true")
		}
		if len(c.Updates.SilenceDurations) == 0 {
			return errors.New("updates.silence_durations must have at least one entry when updates.enabled is true")
		}
		for _, d := range c.Updates.SilenceDurations {
			if _, err := time.ParseDuration(d); err != nil {
				return fmt.Errorf("updates.silence_durations: invalid duration %q: %w", d, err)
			}
		}
		if c.Updates.LabelCacheTTL <= 0 {
			return errors.New("updates.label_cache_ttl must be > 0 when updates.enabled is true")
		}
		if c.Updates.LabelCacheMax <= 0 {
			return errors.New("updates.label_cache_max must be > 0 when updates.enabled is true")
		}
		if c.Alertmanager.URL == "" {
			return errors.New("alertmanager.url is required when updates.enabled is true")
		}
		if c.Alertmanager.RequestTimeout <= 0 {
			return errors.New("alertmanager.request_timeout must be > 0 when updates.enabled is true")
		}
		if c.Updates.ButtonTTL < ButtonTTLMin || c.Updates.ButtonTTL > ButtonTTLMax {
			return fmt.Errorf("updates.button_ttl must be within %s..%s, got %s",
				ButtonTTLMin, ButtonTTLMax, c.Updates.ButtonTTL)
		}
	}
	return nil
}

func Path() string {
	if v := os.Getenv("ALERTLY_CONFIG"); v != "" {
		return v
	}
	return DefaultPath
}

func DryRun() bool {
	switch os.Getenv("DRY_RUN") {
	case "1", "true", "TRUE", "True", "yes":
		return true
	}
	return false
}

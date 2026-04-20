package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	p := writeFile(t, `server:
  listen_addr: ":9090"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("listen_addr override failed: %q", cfg.Server.ListenAddr)
	}
	if cfg.Server.ReadTimeout != 10*time.Second {
		t.Errorf("default read_timeout lost: %v", cfg.Server.ReadTimeout)
	}
	if cfg.Templates["default"] == "" {
		t.Error("default template not populated")
	}
	if cfg.Telegram.APIURL == "" {
		t.Error("default api url lost")
	}
}

func TestLoadFull(t *testing.T) {
	p := writeFile(t, `server:
  listen_addr: ":8080"
  read_timeout: 5s
  write_timeout: 30s
  shutdown_timeout: 15s
  max_body_bytes: 524288
telegram:
  api_url: "https://api.telegram.org"
  parse_mode: "HTML"
  request_timeout: 7s
  rate_limit:
    per_chat_per_sec: 2
    global_per_sec: 25
  retry:
    max_attempts: 3
    initial_backoff: 500ms
    max_backoff: 30s
templates:
  default: "{{ .Title }}"
  custom: "X {{ .Title }}"
logging:
  level: "debug"
  format: "text"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Telegram.RateLimit.PerChatPerSec != 2 {
		t.Errorf("per_chat: %v", cfg.Telegram.RateLimit.PerChatPerSec)
	}
	if cfg.Templates["custom"] != "X {{ .Title }}" {
		t.Errorf("custom template lost")
	}
	if cfg.Server.ReadTimeout != 5*time.Second {
		t.Errorf("read_timeout: %v", cfg.Server.ReadTimeout)
	}
}

func TestLogLevelEnvOverride(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")
	p := writeFile(t, `logging:
  level: "info"
  format: "json"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("env override failed: %q", cfg.Logging.Level)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"bad level", `logging: {level: "fatal", format: "json"}`},
		{"bad format", `logging: {level: "info", format: "csv"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeFile(t, c.yaml)
			if _, err := Load(p); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestPathFromEnv(t *testing.T) {
	t.Setenv("ALERTLY_CONFIG", "/custom/path.yaml")
	if Path() != "/custom/path.yaml" {
		t.Error("path from env failed")
	}
	t.Setenv("ALERTLY_CONFIG", "")
	if Path() != DefaultPath {
		t.Error("default path failed")
	}
}

func TestDryRun(t *testing.T) {
	t.Setenv("DRY_RUN", "true")
	if !DryRun() {
		t.Error("dry run true expected")
	}
	t.Setenv("DRY_RUN", "no")
	if DryRun() {
		t.Error("dry run false expected")
	}
}

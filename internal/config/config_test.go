package config

import (
	"path/filepath"
	"testing"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 300 || !cfg.Ignition.Enabled || cfg.Ignition.StartHour != 7 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
	if !cfg.Provider("codex").Ignite || cfg.Provider("antigravity").Ignite {
		t.Fatal("codex ignites by default, antigravity does not")
	}
}

func TestPartialProviderBlockKeepsDefaults(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\npriority: 1\nproviders:\n  antigravity:\n    ignite: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	ag := cfg.Provider("antigravity")
	if !ag.Ignite || !ag.Monitor || len(ag.Groups) != 1 || ag.Groups[0] != "Gemini" {
		t.Fatalf("antigravity %+v", ag)
	}
	if !cfg.Provider("claude").Ignite {
		t.Fatal("other providers keep their defaults")
	}
}

func TestParseClampsValues(t *testing.T) {
	cfg, err := Parse([]byte("poll_interval_seconds: 5\nignition:\n  start_hour: 30\n  max_transient_failures: 50\ncodex_reset_updates:\n  poll_seconds: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 60 || cfg.Ignition.StartHour != 23 || cfg.Ignition.MaxTransientFailures != 10 || cfg.CodexResetUpdates.PollSeconds != 300 {
		t.Fatalf("not clamped: %+v", cfg)
	}
	if cfg.Ignition.EndHour != 22 {
		t.Fatal("keys missing from a partial block keep their defaults")
	}
}

func TestParseRejectsInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("bark_url: [")); err == nil {
		t.Fatal("invalid YAML must fail")
	}
}

func TestHostConfigPath(t *testing.T) {
	if got := HostConfigPath([]string{"./CLIProxyAPI", "-config", "/etc/cpa.yaml"}, "/w"); got != "/etc/cpa.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := HostConfigPath([]string{"./CLIProxyAPI", "--config=/etc/x.yaml"}, "/w"); got != "/etc/x.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := HostConfigPath([]string{"./CLIProxyAPI"}, "/w"); got != filepath.Join("/w", "config.yaml") {
		t.Fatalf("got %q", got)
	}
}

func TestPluginEnabled(t *testing.T) {
	enabled, err := PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    lamplighter:\n      enabled: true\n"))
	if err != nil || !enabled {
		t.Fatalf("got %v %v", enabled, err)
	}
	enabled, _ = PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    lamplighter:\n      enabled: false\n"))
	if enabled {
		t.Fatal("disabled plugin reported as enabled")
	}
}

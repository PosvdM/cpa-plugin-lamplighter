// Package config parses the plugins.configs.lamplighter subtree of the CPA config.
package config

import (
	// Embedded so that timezone works without tzdata in the container.
	_ "time/tzdata"

	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PluginID is the CPA plugin ID and the key under plugins.configs.
const PluginID = "lamplighter"

// DefaultBarkIcon is the Lamplighter logo.
const DefaultBarkIcon = "https://raw.githubusercontent.com/PosvdM/cpa-plugin-lamplighter/main/assets/logo.png"

// oldBarkIcon is the CPA Management Center logo, the default before v0.1.17.
// Saved configs that still hold it get the new default.
const oldBarkIcon = "https://raw.githubusercontent.com/router-for-me/Cli-Proxy-API-Management-Center/main/logo.jpg"

// Provider holds the per-provider monitoring and ignition switches.
type Provider struct {
	Monitor bool              `yaml:"monitor" json:"monitor"`
	Ignite  bool              `yaml:"ignite" json:"ignite"`
	Model   string            `yaml:"model" json:"model"`
	Groups  []string          `yaml:"groups,omitempty" json:"groups,omitempty"`
	Models  map[string]string `yaml:"models,omitempty" json:"models,omitempty"`
}

// Ignition holds the daily ignition window and failure protection settings.
type Ignition struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	StartHour                int  `yaml:"start_hour" json:"start_hour"`
	EndHour                  int  `yaml:"end_hour" json:"end_hour"`
	EndGraceMinutes          int  `yaml:"end_grace_minutes" json:"end_grace_minutes"`
	GraceSeconds             int  `yaml:"grace_seconds" json:"grace_seconds"`
	FailureRetrySeconds      int  `yaml:"failure_retry_seconds" json:"failure_retry_seconds"`
	MaxTransientFailures     int  `yaml:"max_transient_failures" json:"max_transient_failures"`
	FailureBackoffMultiplier int  `yaml:"failure_backoff_multiplier" json:"failure_backoff_multiplier"`
	PostSuccessHoldSeconds   int  `yaml:"post_success_hold_seconds" json:"post_success_hold_seconds"`
}

// CodexResetUpdates controls forwarding of Did Codex Reset signals.
type CodexResetUpdates struct {
	Enabled              bool `yaml:"enabled" json:"enabled"`
	PollSeconds          int  `yaml:"poll_seconds" json:"poll_seconds"`
	NotifyCurrentPending bool `yaml:"notify_current_pending" json:"notify_current_pending"`
}

// Config is the effective plugin configuration.
type Config struct {
	BarkURL              string  `yaml:"bark_url" json:"bark_url"`
	BarkGroup            string  `yaml:"bark_group" json:"bark_group"`
	BarkIcon             string  `yaml:"bark_icon" json:"bark_icon"`
	NoticeThreshold      float64 `yaml:"notice_threshold" json:"notice_threshold"`
	LowThreshold         float64 `yaml:"low_threshold" json:"low_threshold"`
	CriticalThreshold    float64 `yaml:"critical_threshold" json:"critical_threshold"`
	NotifyRecovery       bool    `yaml:"notify_recovery" json:"notify_recovery"`
	NotifyResetReminders bool    `yaml:"notify_reset_reminders" json:"notify_reset_reminders"`
	// Timezone is an IANA name such as Asia/Shanghai. When both Timezone and
	// TimezoneOffsetHours are empty, the plugin uses the time zone of the CPA
	// process.
	Timezone            string   `yaml:"timezone" json:"timezone"`
	TimezoneOffsetHours *float64 `yaml:"timezone_offset_hours,omitempty" json:"timezone_offset_hours,omitempty"`

	PollIntervalSeconds   int `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	PassiveSkipSeconds    int `yaml:"passive_skip_seconds" json:"passive_skip_seconds"`
	PassiveSkipMaxMinutes int `yaml:"passive_skip_max_minutes" json:"passive_skip_max_minutes"`

	CPABaseURL   string `yaml:"cpa_base_url" json:"cpa_base_url"`
	ModelsAPIKey string `yaml:"models_api_key" json:"models_api_key"`

	Ignition          Ignition            `yaml:"ignition" json:"ignition"`
	Providers         map[string]Provider `yaml:"providers" json:"providers"`
	CodexResetUpdates CodexResetUpdates   `yaml:"codex_reset_updates" json:"codex_reset_updates"`

	HistoryRetentionDays int    `yaml:"history_retention_days" json:"history_retention_days"`
	DataDir              string `yaml:"data_dir" json:"data_dir"`
}

// SupportedProviders lists the providers Lamplighter monitors, in display order.
var SupportedProviders = []string{"codex", "claude", "antigravity"}

// DefaultProvider returns the built-in settings for provider.
func DefaultProvider(provider string) Provider {
	switch provider {
	case "codex", "claude":
		return Provider{Monitor: true, Ignite: true}
	case "antigravity":
		// The Claude / GPT bucket is much smaller than the Gemini one, so only
		// Gemini is eligible for ignition unless groups says otherwise.
		return Provider{Monitor: true, Ignite: false, Groups: []string{"Gemini"}}
	default:
		return Provider{}
	}
}

// Default returns the configuration used when a key is absent.
func Default() Config {
	providers := make(map[string]Provider, len(SupportedProviders))
	for _, name := range SupportedProviders {
		providers[name] = DefaultProvider(name)
	}
	return Config{
		BarkGroup:             "CPA",
		BarkIcon:              DefaultBarkIcon,
		NoticeThreshold:       50,
		LowThreshold:          20,
		CriticalThreshold:     10,
		NotifyRecovery:        false,
		NotifyResetReminders:  false,
		PollIntervalSeconds:   300,
		RequestTimeoutSeconds: 20,
		PassiveSkipSeconds:    60,
		PassiveSkipMaxMinutes: 30,
		CPABaseURL:            "http://127.0.0.1:8317",
		Ignition: Ignition{
			Enabled:                  true,
			StartHour:                7,
			EndHour:                  22,
			EndGraceMinutes:          30,
			GraceSeconds:             3,
			FailureRetrySeconds:      300,
			MaxTransientFailures:     3,
			FailureBackoffMultiplier: 3,
			PostSuccessHoldSeconds:   60,
		},
		Providers: providers,
		CodexResetUpdates: CodexResetUpdates{
			Enabled:              false,
			PollSeconds:          300,
			NotifyCurrentPending: true,
		},
		HistoryRetentionDays: 40,
	}
}

// Parse reads the plugin YAML subtree on top of the defaults. Keys that CPA
// owns, such as enabled and priority, are ignored.
func Parse(raw []byte) (Config, error) {
	cfg := Default()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Default(), fmt.Errorf("parse plugin config: %w", err)
		}
		var overlay struct {
			Providers map[string]yaml.Node `yaml:"providers"`
		}
		if err := yaml.Unmarshal(raw, &overlay); err != nil {
			return Default(), fmt.Errorf("parse plugin config: %w", err)
		}
		// Each provider block starts from its own defaults so that a partial
		// block such as {ignite: true} keeps the other keys.
		cfg.Providers = make(map[string]Provider, len(SupportedProviders))
		for _, name := range SupportedProviders {
			cfg.Providers[name] = DefaultProvider(name)
		}
		for name, node := range overlay.Providers {
			key := strings.ToLower(strings.TrimSpace(name))
			provider := DefaultProvider(key)
			if err := node.Decode(&provider); err != nil {
				return Default(), fmt.Errorf("parse providers.%s: %w", name, err)
			}
			cfg.Providers[key] = provider
		}
	}
	cfg.normalize()
	return cfg, nil
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if maximum > minimum && value > maximum {
		return maximum
	}
	return value
}

func (c *Config) normalize() {
	c.BarkURL = strings.TrimRight(strings.TrimSpace(c.BarkURL), "/")
	c.BarkGroup = strings.TrimSpace(c.BarkGroup)
	c.BarkIcon = strings.TrimSpace(c.BarkIcon)
	if c.BarkIcon == oldBarkIcon {
		c.BarkIcon = DefaultBarkIcon
	}
	c.CPABaseURL = strings.TrimRight(strings.TrimSpace(c.CPABaseURL), "/")
	if c.CPABaseURL == "" {
		c.CPABaseURL = "http://127.0.0.1:8317"
	}
	c.ModelsAPIKey = strings.TrimSpace(c.ModelsAPIKey)
	c.DataDir = strings.TrimSpace(c.DataDir)
	c.Timezone = strings.TrimSpace(c.Timezone)
	if c.TimezoneOffsetHours != nil && (*c.TimezoneOffsetHours < -14 || *c.TimezoneOffsetHours > 14) {
		c.TimezoneOffsetHours = nil
	}
	c.PollIntervalSeconds = clamp(c.PollIntervalSeconds, 60, 0)
	c.RequestTimeoutSeconds = clamp(c.RequestTimeoutSeconds, 5, 120)
	c.PassiveSkipSeconds = clamp(c.PassiveSkipSeconds, 0, 0)
	c.PassiveSkipMaxMinutes = clamp(c.PassiveSkipMaxMinutes, 0, 0)
	c.Ignition.StartHour = clamp(c.Ignition.StartHour, 0, 23)
	c.Ignition.EndHour = clamp(c.Ignition.EndHour, 0, 23)
	c.Ignition.EndGraceMinutes = clamp(c.Ignition.EndGraceMinutes, 0, 0)
	c.Ignition.GraceSeconds = clamp(c.Ignition.GraceSeconds, 0, 0)
	c.Ignition.FailureRetrySeconds = clamp(c.Ignition.FailureRetrySeconds, 60, 0)
	c.Ignition.MaxTransientFailures = clamp(c.Ignition.MaxTransientFailures, 1, 10)
	c.Ignition.FailureBackoffMultiplier = clamp(c.Ignition.FailureBackoffMultiplier, 1, 10)
	c.Ignition.PostSuccessHoldSeconds = clamp(c.Ignition.PostSuccessHoldSeconds, 15, 0)
	c.CodexResetUpdates.PollSeconds = clamp(c.CodexResetUpdates.PollSeconds, 300, 0)
	c.HistoryRetentionDays = clamp(c.HistoryRetentionDays, 1, 365)
	for name, provider := range c.Providers {
		provider.Model = strings.TrimSpace(provider.Model)
		c.Providers[name] = provider
	}
}

// Location returns the time zone used for display and the ignition window:
// timezone when set, then timezone_offset_hours, then the time zone of the
// CPA process (the official Docker image sets it from TZ).
func (c Config) Location() *time.Location {
	if c.Timezone != "" {
		if loc, err := time.LoadLocation(c.Timezone); err == nil {
			return loc
		}
	}
	if c.TimezoneOffsetHours != nil {
		hours := *c.TimezoneOffsetHours
		return time.FixedZone(fmt.Sprintf("UTC%+g", hours), int(hours*3600))
	}
	return time.Local
}

// Provider returns the settings for provider, falling back to its defaults.
func (c Config) Provider(name string) Provider {
	if provider, ok := c.Providers[name]; ok {
		return provider
	}
	return DefaultProvider(name)
}

// PollInterval returns the quota polling interval.
func (c Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

// RequestTimeout returns the timeout for one upstream HTTP request.
func (c Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

package config

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostConfigPath returns the CPA config file path. CPA reads it from the
// -config flag and falls back to config.yaml in the working directory.
func HostConfigPath(args []string, workDir string) string {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name != "config" {
			continue
		}
		if !hasValue && i+1 < len(args) {
			value = args[i+1]
		}
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return filepath.Join(workDir, "config.yaml")
}

// PluginEnabled reports whether the CPA config enables plugins and this
// plugin. CPA keeps a disabled plugin loaded without notifying it, so the
// plugin checks the flag itself before doing any work.
func PluginEnabled(raw []byte) (bool, error) {
	var cfg struct {
		Plugins struct {
			Enabled bool `yaml:"enabled"`
			Configs map[string]struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false, err
	}
	return cfg.Plugins.Enabled && cfg.Plugins.Configs[PluginID].Enabled, nil
}

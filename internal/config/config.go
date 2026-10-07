// Package config loads the agent's on-disk configuration. Flags passed to the
// agent override file values; the installer writes this file so the service
// starts with the right token, bind address and data directory.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the agent's persisted configuration.
type Config struct {
	// Addr is the listen address. Bind to the tailnet IP in production.
	Addr string `json:"addr"`
	// Token is the bearer secret for the control channel.
	Token string `json:"token"`
	// DataDir holds the audit log and dynamic-endpoint store.
	DataDir string `json:"data_dir"`
	// Allow, if non-empty, restricts callable methods to this set.
	Allow []string `json:"allow,omitempty"`
	// Deny always rejects these methods.
	Deny []string `json:"deny,omitempty"`
}

// DefaultPath returns the conventional config path for the current OS.
func DefaultPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "Astrea", "config.json")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "astrea", "config.json")
	}
	return "/etc/astrea/config.json"
}

// DefaultDataDir returns the conventional data directory for the current OS.
func DefaultDataDir() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "Astrea")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "astrea")
	}
	return "/var/lib/astrea"
}

// Load reads a config file. A missing file yields a zero Config and no error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config as indented JSON, creating parent directories.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

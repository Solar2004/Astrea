// Package config loads the agent's on-disk configuration. Flags passed to the
// agent override file values; the installer writes this file so the service
// starts with the right token, bind address and data directory.
package config

import (
	"bytes"
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
	// Mode is the default permission profile: "yolo" (default) lets the AI
	// call every capability; "readonly" only allows observing the machine
	// (files, stats, screen, logs). Allow/Deny refine either one.
	Mode string `json:"mode,omitempty"`
	// KeepAwake is the sleep inhibitor mode: "auto" (default; awake while a
	// service, job or download runs), "always" or "off".
	KeepAwake string `json:"keep_awake,omitempty"`
	// AutoUpdate controls self-updates from GitHub Releases: "auto" (default;
	// checks every few hours and installs when no job or download is running),
	// "notify" (only reports a newer version in admin.status) or "off".
	AutoUpdate string `json:"auto_update,omitempty"`
	// BrowserHeadless runs browser.* with no visible window. Default false:
	// the owner and the live view see what the AI does in the browser.
	BrowserHeadless bool `json:"browser_headless,omitempty"`
	// Vision points vision.locate at a local GUI-grounding model served behind
	// an OpenAI-compatible endpoint (e.g. UI-TARS on vLLM). Empty = disabled.
	Vision VisionConfig `json:"vision,omitempty"`
}

// VisionConfig configures the layer-3 vision model (see capability.VisionConfig).
type VisionConfig struct {
	Endpoint    string   `json:"endpoint,omitempty"`
	Model       string   `json:"model,omitempty"`
	APIKey      string   `json:"api_key,omitempty"`
	Serve       []string `json:"serve,omitempty"`
	IdleStopSec int      `json:"idle_stop_sec,omitempty"`
	Coords      string   `json:"coords,omitempty"`
}

// Permission modes.
const (
	ModeYolo     = "yolo"
	ModeReadOnly = "readonly"
)

// ReadOnlyAllow is the allow list applied by ModeReadOnly.
var ReadOnlyAllow = []string{
	"fs.list", "fs.read", "sys.*", "proc.list", "screen.capture",
	"task.list", "task.get", "endpoint.list", "deps.check",
	"service.status", "service.logs", "job.status", "job.logs",
	"net.download.status", "memory.get",
	"clipboard.get", "clipboard.history", "schedule.list", "browser.read",
}

// EffectiveAllow returns the allow list after applying Mode. An unknown mode
// is treated as readonly, so a typo never grants more than intended.
func (c *Config) EffectiveAllow() []string {
	switch c.Mode {
	case "", ModeYolo:
		return c.Allow
	}
	return append(append([]string{}, ReadOnlyAllow...), c.Allow...)
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
	// Windows PowerShell 5.1 writes UTF-8 with a BOM, which encoding/json rejects.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
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

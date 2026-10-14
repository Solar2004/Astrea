package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Solar2004/Astrea/internal/config"
)

// runDiagnose prints a self-contained report the user can copy to us when
// something goes wrong: version, the effective config with the token redacted,
// and the tail of the agent and audit logs. It never prints the token.
func runDiagnose(cfgPath string) {
	out := os.Stdout
	fmt.Fprintf(out, "=== astrea-agent diagnose ===\n")
	fmt.Fprintf(out, "version:   %s\n", version)
	fmt.Fprintf(out, "platform:  %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(out, "config:    %s\n", cfgPath)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(out, "config ERROR: %v\n", err)
		return
	}
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = config.DefaultDataDir()
	}
	mode := cfg.Mode
	if mode == "" {
		mode = config.ModeYolo
	}
	fmt.Fprintf(out, "addr:      %s\n", cfg.Addr)
	fmt.Fprintf(out, "data_dir:  %s\n", dataDir)
	fmt.Fprintf(out, "mode:      %s\n", mode)
	fmt.Fprintf(out, "keep_awake:%s\n", orDefault(cfg.KeepAwake, "auto"))
	fmt.Fprintf(out, "token set: %v\n", cfg.Token != "")
	if len(cfg.Allow) > 0 {
		fmt.Fprintf(out, "allow:     %s\n", strings.Join(cfg.Allow, ", "))
	}
	if len(cfg.Deny) > 0 {
		fmt.Fprintf(out, "deny:      %s\n", strings.Join(cfg.Deny, ", "))
	}

	dir := filepath.Dir(cfgPath)
	for _, name := range []string{"agent.log", "agent-gui.log", "audit.log"} {
		printTail(out, filepath.Join(dir, name), 40)
		if dir != dataDir {
			printTail(out, filepath.Join(dataDir, name), 40)
		}
	}

	// A compact machine-readable copy helps us parse it quickly.
	report := map[string]any{
		"version": version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"addr": cfg.Addr, "data_dir": dataDir, "mode": mode, "token_set": cfg.Token != "",
	}
	b, _ := json.Marshal(report)
	fmt.Fprintf(out, "\n=== json ===\n%s\n", b)
}

// printTail writes the last n lines of path, if it exists.
func printTail(out *os.File, path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintf(out, "\n--- %s (last %d lines) ---\n%s\n", path, len(lines), strings.Join(lines, "\n"))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

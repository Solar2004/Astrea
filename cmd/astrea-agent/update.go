package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/update"
)

// restartRequested makes main start a fresh copy of the (just replaced)
// binary once this process has shut down cleanly.
var restartRequested atomic.Bool

// restarting makes sure only one fresh copy is started.
var restarting atomic.Bool

// requestRestart asks the agent to shut down and come back up.
func requestRestart(stop func()) {
	restartRequested.Store(true)
	go func() {
		// Let the result of the call that asked for it reach the hub first.
		time.Sleep(500 * time.Millisecond)
		stop()
	}()
}

// restartAfterExit brings the agent back after a requested restart: systemd
// does it by itself on our exit code, otherwise we spawn the new binary.
func restartAfterExit(logger *log.Logger) bool {
	if !restartRequested.Load() || !restarting.CompareAndSwap(false, true) {
		return false // not asked, or already done (normal exit vs. the shutdown watchdog)
	}
	if update.Supervised() {
		logger.Printf("restarting: exit %d, systemd starts the new binary", update.ExitRestart)
		os.Exit(update.ExitRestart)
	}
	exe, err := update.Self()
	if err == nil {
		err = update.Respawn(exe, os.Args[1:])
	}
	if err != nil {
		logger.Printf("restart failed: %v (the agent comes back at the next boot/logon)", err)
		os.Exit(1)
	}
	logger.Printf("restarting: spawned %s", exe)
	return true
}

// updater checks GitHub Releases and installs newer agent binaries.
type updater struct {
	mode   string // auto | notify | off
	busy   func() bool
	stop   func()
	gui    func(ctx context.Context) error // restarts the gui helper, if any
	logger *log.Logger

	mu       sync.Mutex
	latest   string
	checked  time.Time
	lastErr  string
	updating bool
}

// status is merged into admin.status so the hub and panel see versions.
func (u *updater) status() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := map[string]any{"version": version, "auto_update": u.mode}
	if u.latest != "" {
		st["latest_version"] = u.latest
		st["update_available"] = update.Newer(version, u.latest)
	}
	if !u.checked.IsZero() {
		st["update_checked"] = u.checked.UTC().Format(time.RFC3339)
	}
	if u.lastErr != "" {
		st["update_error"] = u.lastErr
	}
	return st
}

func (u *updater) check(ctx context.Context) (string, error) {
	tag, err := update.Latest(ctx, update.Base())
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = time.Now()
	if err != nil {
		u.lastErr = err.Error()
		return "", err
	}
	u.latest, u.lastErr = tag, ""
	return tag, nil
}

// apply installs tag over the running binary and schedules a restart.
func (u *updater) apply(ctx context.Context, tag string, progress update.Progress) error {
	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		return fmt.Errorf("an update is already in progress")
	}
	u.updating = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.updating = false; u.mu.Unlock() }()

	exe, err := update.Self()
	if err != nil {
		return err
	}
	if err := update.Apply(ctx, update.Base(), "astrea-agent", tag, exe, progress); err != nil {
		u.mu.Lock()
		u.lastErr = err.Error()
		u.mu.Unlock()
		return err
	}
	u.logger.Printf("updated %s -> %s; restarting", version, tag)
	if u.gui != nil {
		if err := u.gui(ctx); err != nil {
			u.logger.Printf("gui helper restart: %v (it picks up the new binary at the next logon)", err)
		}
	}
	requestRestart(u.stop)
	return nil
}

// loop checks periodically and, in auto mode, installs when idle.
func (u *updater) loop(ctx context.Context) {
	if u.mode == "off" || version == "dev" {
		return
	}
	// First check a few minutes after start (Tailscale, services settle),
	// then every 6h with jitter so a fleet does not hit GitHub at once.
	wait := 3*time.Minute + time.Duration(rand.Int64N(int64(2*time.Minute)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 6*time.Hour + time.Duration(rand.Int64N(int64(time.Hour)))
		tag, err := u.check(ctx)
		if err != nil {
			u.logger.Printf("update check: %v", err)
			continue
		}
		if !update.Newer(version, tag) || u.mode != "auto" {
			continue
		}
		if u.busy() {
			u.logger.Printf("update %s available; waiting until no job or download is running", tag)
			wait = 20 * time.Minute
			continue
		}
		if err := u.apply(ctx, tag, func(s string) { u.logger.Print("update: " + s) }); err != nil {
			u.logger.Printf("auto-update to %s: %v", tag, err)
		}
	}
}

// agentUpdate is the agent.update capability.
type agentUpdate struct{ u *updater }

func (agentUpdate) Name() string { return "agent.update" }

func (agentUpdate) Describe() capability.Descriptor {
	return capability.Descriptor{
		Name: "agent.update",
		Description: "Update the Astrea agent on this machine to the latest release (or a given version) from GitHub, " +
			"verified with the release checksums, then restart it (~5 s offline; supervised services come back by themselves, " +
			"running jobs are killed). check_only=true just reports current and latest versions.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"check_only": {"type": "boolean", "description": "only report versions, install nothing"},
				"version": {"type": "string", "description": "release tag to install, e.g. v0.3.0 (default: latest)"},
				"force": {"type": "boolean", "description": "install even if not newer (reinstall/downgrade) or if jobs are running"}
			}
		}`),
	}
}

func (a agentUpdate) Invoke(ctx context.Context, payload json.RawMessage, emit capability.Emit) (any, error) {
	var req struct {
		CheckOnly bool   `json:"check_only"`
		Version   string `json:"version"`
		Force     bool   `json:"force"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("bad payload: %w", err)
		}
	}
	tag := req.Version
	latest, err := a.u.check(ctx)
	if tag == "" {
		if err != nil {
			return nil, err
		}
		tag = latest
	}
	res := map[string]any{"current": version, "latest": latest, "target": tag, "updated": false}
	if req.CheckOnly {
		res["update_available"] = update.Newer(version, tag)
		return res, nil
	}
	if !req.Force && !update.Newer(version, tag) {
		res["note"] = "already up to date (force=true to reinstall)"
		return res, nil
	}
	if !req.Force && a.u.busy() {
		return nil, fmt.Errorf("a job or download is running and would be killed by the restart; retry later or use force=true")
	}
	progress := func(s string) { emit(protocol.StreamChunk{Channel: "stdout", Data: s + "\n"}) }
	if err := a.u.apply(ctx, tag, progress); err != nil {
		return nil, err
	}
	res["updated"] = true
	res["note"] = "restarting now; the agent is back in a few seconds"
	return res, nil
}

// agentRestart is agent.restart: on the gui helper it is how the headless
// agent makes it load a new binary; on the main agent it is a plain restart.
type agentRestart struct{ stop func() }

func (agentRestart) Name() string { return "agent.restart" }

func (agentRestart) Describe() capability.Descriptor {
	return capability.Descriptor{
		Name:        "agent.restart",
		Description: "Restart the Astrea agent process (it is back in a few seconds). Running jobs are killed; services restart.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (a agentRestart) Invoke(context.Context, json.RawMessage, capability.Emit) (any, error) {
	requestRestart(a.stop)
	return map[string]any{"restarting": true}, nil
}

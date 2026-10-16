package capability

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VisionManager owns the lifecycle of the local vision model process. The model
// is heavy (several GB of VRAM), so by default it is NOT running: the AI starts
// it on demand (vision.start, or the first vision.locate), it works, and it is
// stopped again — manually (vision.stop) or automatically after IdleStopSec
// with no use, freeing the GPU. The agent process itself stays tiny; only this
// child holds the GPU, and only while in use.
type VisionManager struct {
	cfg VisionConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	started time.Time
	lastUse time.Time
	logf    func(string, ...any)
}

// NewVisionManager builds the manager. logf may be nil.
func NewVisionManager(cfg VisionConfig, logf func(string, ...any)) *VisionManager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &VisionManager{cfg: cfg, logf: logf}
}

// Configured reports whether a model endpoint is set at all.
func (m *VisionManager) Configured() bool { return m.cfg.Endpoint != "" }

// canServe reports whether we know how to launch the model ourselves.
func (m *VisionManager) canServe() bool { return len(m.cfg.Serve) > 0 }

func (m *VisionManager) running() bool {
	return m.cmd != nil && m.cmd.Process != nil && m.cmd.ProcessState == nil
}

// baseURL derives the server root from the chat/completions endpoint, so we can
// poll /v1/models for readiness.
func (m *VisionManager) modelsURL() string {
	e := m.cfg.Endpoint
	if i := strings.Index(e, "/chat/completions"); i >= 0 {
		return e[:i] + "/models"
	}
	return e
}

// ready reports whether the server answers (model loaded).
func (m *VisionManager) ready(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", m.modelsURL(), nil)
	if err != nil {
		return false
	}
	if m.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// Start launches the model (if not already up) and waits up to waitSec for it
// to answer. A model that is already running (started elsewhere) is detected by
// ready() and reused. Returns when ready, or an error on timeout.
func (m *VisionManager) Start(ctx context.Context, waitSec int) error {
	if !m.Configured() {
		return errNoVision
	}
	if m.ready(ctx) {
		m.touch()
		return nil
	}
	if !m.canServe() {
		return fmt.Errorf("the vision model is not running and no 'serve' command is configured to start it (set vision.serve in config)")
	}
	m.mu.Lock()
	if !m.running() {
		cmd := exec.Command(m.cfg.Serve[0], m.cfg.Serve[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("launch vision model: %w", err)
		}
		m.cmd, m.started = cmd, time.Now()
		go func() { _ = cmd.Wait() }()
		m.logf("vision: started model (pid %d), waiting for it to load", cmd.Process.Pid)
	}
	m.mu.Unlock()

	if waitSec <= 0 {
		waitSec = 180 // first load (weights → GPU) can take a while
	}
	deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if m.ready(ctx) {
			m.touch()
			m.logf("vision: model ready")
			return nil
		}
		m.mu.Lock()
		dead := !m.running()
		m.mu.Unlock()
		if dead {
			return fmt.Errorf("vision model process exited while loading (check its log / VRAM)")
		}
	}
	return fmt.Errorf("vision model did not become ready within %ds", waitSec)
}

// Stop kills the model process, freeing the GPU. No-op if we did not start it.
func (m *VisionManager) Stop() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running() {
		m.cmd = nil
		return false
	}
	_ = m.cmd.Process.Kill()
	m.logf("vision: stopped model (freeing the GPU)")
	m.cmd = nil
	return true
}

func (m *VisionManager) touch() {
	m.mu.Lock()
	m.lastUse = time.Now()
	m.mu.Unlock()
}

// Loop auto-stops the model after IdleStopSec with no use.
func (m *VisionManager) Loop(ctx context.Context) {
	idle := m.cfg.IdleStopSec
	if idle <= 0 {
		idle = 300
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Stop()
			return
		case <-t.C:
		}
		m.mu.Lock()
		stale := m.running() && !m.lastUse.IsZero() && time.Since(m.lastUse) > time.Duration(idle)*time.Second
		m.mu.Unlock()
		if stale {
			m.logf("vision: idle for over %ds, stopping to free the GPU", idle)
			m.Stop()
		}
	}
}

// Status reports whether the model runs and, when it does, its resource use.
func (m *VisionManager) Status(ctx context.Context) map[string]any {
	m.mu.Lock()
	running := m.running()
	var pid int
	var started, last time.Time
	if m.cmd != nil && m.cmd.Process != nil {
		pid = m.cmd.Process.Pid
	}
	started, last = m.started, m.lastUse
	m.mu.Unlock()

	idle := m.cfg.IdleStopSec
	if idle <= 0 {
		idle = 300
	}
	out := map[string]any{
		"configured":    m.Configured(),
		"can_serve":     m.canServe(),
		"running":       running,
		"ready":         m.Configured() && m.ready(ctx),
		"idle_stop_sec": idle,
		"model":         m.cfg.Model,
		"endpoint":      m.cfg.Endpoint,
	}
	if running {
		out["pid"] = pid
		if !started.IsZero() {
			out["uptime_sec"] = int(time.Since(started).Seconds())
		}
		if !last.IsZero() {
			out["idle_sec"] = int(time.Since(last).Seconds())
		}
		if ram := procRAMMB(pid); ram > 0 {
			out["ram_mb"] = ram
		}
	}
	if v := gpuVRAM(ctx); v != "" {
		out["gpu"] = v
	}
	return out
}

// procRAMMB returns the resident memory of a pid in MB (best effort).
func procRAMMB(pid int) int {
	if pid == 0 {
		return 0
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("[int]((Get-Process -Id %d).WorkingSet64/1MB)", pid)).Output()
		if err == nil {
			n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
			return n
		}
		return 0
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	rss, _ := strconv.Atoi(fields[1])
	return rss * os.Getpagesize() / (1 << 20)
}

// gpuVRAM returns a short "used/total MiB" string from nvidia-smi, if present.
func gpuVRAM(ctx context.Context) string {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--query-gpu=memory.used,memory.total",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return ""
	}
	return strings.TrimSpace(parts[0]) + "/" + strings.TrimSpace(parts[1]) + " MiB"
}

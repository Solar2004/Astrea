package capability

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/download"
)

// Vision auto-provisioning: with one switch on (vision.setup on), the agent
// downloads the local vision model by itself as soon as it has internet —
// resumably, in the background, retrying until done — then wires vision.serve
// automatically. So a freshly updated PC becomes vision-capable on its own,
// without anyone copying 6 GB or editing a config by hand.
//
// What it fetches (defaults, overridable in config):
//   - the model GGUF + its mmproj (both OSes)
//   - on Windows, a llama.cpp CUDA build (llama-server.exe + cuBLAS DLLs)
// and then sets Serve/Endpoint/Coords on the VisionManager.

// Provision URLs. Pinned builds so a format change upstream never breaks a
// silent background download; overridable via VisionConfig.
const (
	defModelURL  = "hf://mradermacher/UI-TARS-1.5-7B-GGUF/UI-TARS-1.5-7B.Q4_K_M.gguf"
	defMMProjURL = "hf://mradermacher/UI-TARS-1.5-7B-GGUF/UI-TARS-1.5-7B.mmproj-Q8_0.gguf"
	// llama.cpp Windows CUDA build (server binary + the cuBLAS runtime DLLs).
	defLlamaURL  = "https://github.com/ggml-org/llama.cpp/releases/download/b11683/llama-b11683-bin-win-cuda-12.4-x64.zip"
	defCudartURL = "https://github.com/ggml-org/llama.cpp/releases/download/b11683/cudart-llama-bin-win-cuda-12.4-x64.zip"
)

// Provisioner downloads and wires up the vision model on demand.
type Provisioner struct {
	Mgr       *VisionManager
	Dir       string // base dir, e.g. <DataDir>/vision
	Dloads    *download.Manager
	StateFn   string // path of the on/off flag file
	Logf      func(string, ...any)
	ModelURL  string
	MMProjURL string
	Ctx       context.Context // agent lifetime; provisioning runs under this, not a call ctx

	mu      sync.Mutex
	on      bool
	running bool
	phase   string // what it is doing now
	errMsg  string
	done    bool
	curDL   string // download in progress, for Builtin's percent
}

func (p *Provisioner) logf(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
	}
}

func (p *Provisioner) modelPath() string  { return filepath.Join(p.Dir, "model.gguf") }
func (p *Provisioner) mmprojPath() string { return filepath.Join(p.Dir, "mmproj.gguf") }
func (p *Provisioner) llamaServer() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(p.Dir, "llama", "llama-server.exe")
	}
	return "llama-server" // Linux: expected on PATH (or set vision.serve by hand)
}

// Load reads the persisted on/off flag and, if on, kicks off provisioning.
func (p *Provisioner) Load() {
	if b, err := os.ReadFile(p.StateFn); err == nil {
		var st struct {
			On bool `json:"on"`
		}
		if json.Unmarshal(b, &st) == nil && st.On {
			p.mu.Lock()
			p.on = true
			p.mu.Unlock()
			go p.loop()
		}
	}
}

// SetOn turns auto-provisioning on or off, persists it, and starts/stops work.
func (p *Provisioner) SetOn(on bool) error {
	p.mu.Lock()
	p.on = on
	p.mu.Unlock()
	b, _ := json.Marshal(map[string]bool{"on": on})
	if err := os.MkdirAll(filepath.Dir(p.StateFn), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.StateFn, b, 0o600); err != nil {
		return err
	}
	if on {
		go p.loop()
	}
	return nil
}

// Status reports provisioning progress.
func (p *Provisioner) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := map[string]any{"on": p.on, "running": p.running, "ready": p.done}
	if p.phase != "" {
		m["phase"] = p.phase
	}
	if p.errMsg != "" {
		m["last_error"] = p.errMsg
	}
	// Report the model files on disk so the UI can show progress.
	if fi, err := os.Stat(p.modelPath()); err == nil {
		m["model_mb"] = fi.Size() / (1 << 20)
	}
	if fi, err := os.Stat(p.mmprojPath()); err == nil {
		m["mmproj_mb"] = fi.Size() / (1 << 20)
	}
	return m
}

// Builtin reports the vision model as one of Astrea's own services.
func (p *Provisioner) Builtin(ctx context.Context) Builtin {
	b := Builtin{Name: "ui-tars", Title: "UI-TARS (vision model)", Required: true}
	p.mu.Lock()
	on, running, done, phase, errMsg, dl := p.on, p.running, p.done, p.phase, p.errMsg, p.curDL
	p.mu.Unlock()
	switch {
	case done && p.Mgr.Running():
		b.State = "running"
	case done:
		b.State = "ready"
	case running && errMsg != "":
		b.State, b.Error = "error", errMsg
	case running:
		b.State, b.Phase = "installing", phase
	case !on && fileExists(p.modelPath()):
		b.State = "paused" // started, then switched off
	default:
		b.State = "missing"
	}
	if b.State == "installing" && dl != "" {
		if s, err := p.Dloads.Get(ctx, dl, 0); err == nil {
			b.Bytes, b.Total, b.ETASec = s.Bytes, s.Total, s.ETASec
			if s.Total > 0 {
				pct := s.Percent
				b.Percent = &pct
			}
		}
	}
	return b
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (p *Provisioner) setPhase(ph string) {
	p.mu.Lock()
	p.phase = ph
	p.mu.Unlock()
	if ph != "" {
		p.logf("vision provision: %s", ph)
	}
}

// loop runs provisioning, retrying with backoff until it succeeds, for as long
// as the switch stays on. "As soon as it has internet": a failed download just
// means retry later.
func (p *Provisioner) loop() {
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.running || !p.on {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	backoff := 30 * time.Second
	for {
		p.mu.Lock()
		on := p.on
		p.mu.Unlock()
		if !on || ctx.Err() != nil {
			return
		}
		err := p.provision(ctx)
		if err == nil {
			p.mu.Lock()
			p.done, p.phase, p.errMsg = true, "ready", ""
			p.mu.Unlock()
			p.logf("vision provision: done, model ready")
			return
		}
		p.mu.Lock()
		p.errMsg = err.Error()
		p.mu.Unlock()
		p.logf("vision provision: %v (retrying in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 10*time.Minute {
			backoff *= 2
		}
	}
}

// provision downloads whatever is missing and wires up Serve. Idempotent:
// files already present (and complete) are skipped; partial ones resume.
func (p *Provisioner) provision(ctx context.Context) error {
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return err
	}
	modelURL, mmURL := p.ModelURL, p.MMProjURL
	if modelURL == "" {
		modelURL = defModelURL
	}
	if mmURL == "" {
		mmURL = defMMProjURL
	}
	// 1. model + mmproj (both OSes).
	p.setPhase("downloading model")
	if err := p.fetch(ctx, modelURL, p.modelPath()); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	p.setPhase("downloading mmproj")
	if err := p.fetch(ctx, mmURL, p.mmprojPath()); err != nil {
		return fmt.Errorf("mmproj: %w", err)
	}
	// 2. runtime (Windows: llama.cpp CUDA).
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(p.llamaServer()); err != nil {
			p.setPhase("downloading llama.cpp (CUDA)")
			zipPath := filepath.Join(p.Dir, "llama.zip")
			cudartPath := filepath.Join(p.Dir, "cudart.zip")
			if err := p.fetch(ctx, defLlamaURL, zipPath); err != nil {
				return fmt.Errorf("llama.cpp: %w", err)
			}
			if err := p.fetch(ctx, defCudartURL, cudartPath); err != nil {
				return fmt.Errorf("cudart: %w", err)
			}
			p.setPhase("unpacking llama.cpp")
			dst := filepath.Join(p.Dir, "llama")
			if err := unzip(zipPath, dst); err != nil {
				return fmt.Errorf("unzip llama: %w", err)
			}
			if err := unzip(cudartPath, dst); err != nil {
				return fmt.Errorf("unzip cudart: %w", err)
			}
			if _, err := os.Stat(p.llamaServer()); err != nil {
				return fmt.Errorf("llama-server.exe not found after unpacking")
			}
		}
	}
	// 3. wire up the manager so vision.* works (unless already configured).
	p.setPhase("configuring")
	p.Mgr.autoConfigure(p.Dir, p.llamaServer())
	return nil
}

// fetch downloads url to dest with the resumable manager, blocking until the
// download finishes (or fails). A present, complete file is skipped.
func (p *Provisioner) fetch(ctx context.Context, url, dest string) error {
	st, err := p.Dloads.Start(download.Request{URL: url, Dest: dest})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.curDL = st.ID
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.curDL = ""; p.mu.Unlock() }()
	for {
		s, err := p.Dloads.Get(ctx, st.ID, 10*time.Second)
		if err != nil {
			return err
		}
		switch s.State {
		case download.Done:
			return nil
		case download.Failed:
			return fmt.Errorf("%s", s.Error)
		case download.Canceled:
			return fmt.Errorf("canceled")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// unzip extracts a zip into dir (flattening nothing; keeps entry paths).
func unzip(src, dir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		fp := filepath.Join(dir, f.Name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(fp, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(fp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

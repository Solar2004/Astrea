// Package endpoint implements dynamic HTTP endpoints that the agent can mount
// at runtime under /dyn/<name>. This is what lets Hermes build "internal APIs
// in real time" on the controlled machine: expose a local AI service, run a
// command on request, or serve a canned response — without restarting.
package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
)

// Kind enumerates the handler types a dynamic endpoint can have.
type Kind string

const (
	// KindExec runs a shell command; the request body is passed on stdin and
	// stdout is returned as the response body.
	KindExec Kind = "exec"
	// KindProxy reverse-proxies the request to a local target URL (e.g. a
	// locally running model server).
	KindProxy Kind = "proxy"
	// KindStatic returns a fixed response body.
	KindStatic Kind = "static"
)

// Spec defines one dynamic endpoint.
type Spec struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Command is used by KindExec. The request body arrives on stdin.
	Command string `json:"command,omitempty"`
	// Target is the base URL used by KindProxy, e.g. http://127.0.0.1:11434.
	Target string `json:"target,omitempty"`
	// Body/ContentType are used by KindStatic.
	Body        string `json:"body,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// Manager stores dynamic endpoints and serves them under a mount prefix.
type Manager struct {
	mu        sync.RWMutex
	endpoints map[string]*Spec
	store     string // optional path to persist specs as JSON
}

// NewManager returns a Manager. If store is non-empty, specs are loaded from it
// and re-saved on every change so they survive agent restarts.
func NewManager(store string) *Manager {
	m := &Manager{endpoints: map[string]*Spec{}, store: store}
	m.load()
	return m
}

// Create registers (or replaces) an endpoint.
func (m *Manager) Create(s *Spec) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch s.Kind {
	case KindExec:
		if s.Command == "" {
			return fmt.Errorf("exec endpoint requires command")
		}
	case KindProxy:
		if _, err := url.Parse(s.Target); err != nil || s.Target == "" {
			return fmt.Errorf("proxy endpoint requires a valid target url")
		}
	case KindStatic:
		// body may be empty
	default:
		return fmt.Errorf("unknown kind %q", s.Kind)
	}
	m.mu.Lock()
	m.endpoints[s.Name] = s
	m.mu.Unlock()
	m.save()
	return nil
}

// Delete removes an endpoint. Returns whether it existed.
func (m *Manager) Delete(name string) bool {
	m.mu.Lock()
	_, ok := m.endpoints[name]
	delete(m.endpoints, name)
	m.mu.Unlock()
	if ok {
		m.save()
	}
	return ok
}

// List returns all specs sorted by name.
func (m *Manager) List() []*Spec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Spec, 0, len(m.endpoints))
	for _, s := range m.endpoints {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ServeHTTP dispatches /dyn/<name>[/...] to the matching endpoint.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, rest := splitName(r.URL.Path)
	if name == "" {
		http.NotFound(w, r)
		return
	}
	m.mu.RLock()
	spec := m.endpoints[name]
	m.mu.RUnlock()
	if spec == nil {
		http.Error(w, "no such endpoint", http.StatusNotFound)
		return
	}
	switch spec.Kind {
	case KindExec:
		m.serveExec(w, r, spec)
	case KindProxy:
		m.serveProxy(w, r, spec, rest)
	case KindStatic:
		ct := spec.ContentType
		if ct == "" {
			ct = "text/plain; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		_, _ = io.WriteString(w, spec.Body)
	}
}

func (m *Manager) serveExec(w http.ResponseWriter, r *http.Request, spec *Spec) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	name, args := shell(spec.Command)
	cmd := exec.CommandContext(ctx, name, args...)
	procutil.NoConsole(cmd)
	cmd.Stdin = r.Body
	cmd.Env = append(os.Environ(), "ASTREA_QUERY="+r.URL.RawQuery, "ASTREA_METHOD="+r.Method)
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "exec failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out)
}

func (m *Manager) serveProxy(w http.ResponseWriter, r *http.Request, spec *Spec, rest string) {
	target, err := url.Parse(spec.Target)
	if err != nil {
		http.Error(w, "bad target", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Rewrite the path so /dyn/<name>/foo -> <target>/foo.
	r.URL.Path = rest
	r.Host = target.Host
	proxy.ServeHTTP(w, r)
}

// splitName extracts the endpoint name and remaining path from /dyn/<name>/...
func splitName(path string) (name, rest string) {
	const prefix = "/dyn/"
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		return "", ""
	}
	p := path[len(prefix):]
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i:]
		}
	}
	return p, "/"
}

func shell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "/bin/sh", []string{"-c", command}
}

func (m *Manager) load() {
	if m.store == "" {
		return
	}
	data, err := os.ReadFile(m.store)
	if err != nil {
		return
	}
	var specs []*Spec
	if json.Unmarshal(data, &specs) != nil {
		return
	}
	for _, s := range specs {
		m.endpoints[s.Name] = s
	}
}

func (m *Manager) save() {
	if m.store == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.store), 0o755)
	data, err := json.MarshalIndent(m.List(), "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.store, data, 0o644)
}

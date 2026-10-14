// Package endpoint implements dynamic HTTP endpoints that the agent can mount
// at runtime under /dyn/<name>. This is what lets Hermes build "internal APIs
// in real time" on the controlled machine: expose a local AI service, run a
// command on request, or serve a canned response — without restarting.
package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/shell"
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
	// KindTask runs a stored task: a JSON object body (or the query string)
	// becomes its arguments, and the response is JSON {result, output}.
	KindTask Kind = "task"
)

// TaskRunner runs a stored task by name (see package task).
type TaskRunner func(ctx context.Context, name string, args json.RawMessage, emit func(protocol.StreamChunk)) (any, error)

// Spec defines one dynamic endpoint.
type Spec struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Command is used by KindExec. The request body arrives on stdin.
	Command string `json:"command,omitempty"`
	// Shell selects the interpreter for Command (see package shell).
	Shell string `json:"shell,omitempty"`
	// Target is the base URL used by KindProxy, e.g. http://127.0.0.1:11434.
	Target string `json:"target,omitempty"`
	// Body/ContentType are used by KindStatic.
	Body        string `json:"body,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// Task is the task name used by KindTask.
	Task string `json:"task,omitempty"`
}

// Manager stores dynamic endpoints and serves them under a mount prefix.
type Manager struct {
	mu        sync.RWMutex
	endpoints map[string]*Spec
	store     string // optional path to persist specs as JSON

	// Tasks runs KindTask endpoints. Set before serving.
	Tasks TaskRunner
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
		u, err := url.Parse(s.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("proxy endpoint requires an http(s) target url with a host, e.g. http://127.0.0.1:11434")
		}
	case KindStatic:
		// body may be empty
	case KindTask:
		if s.Task == "" {
			return fmt.Errorf("task endpoint requires task (the task name)")
		}
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
	case KindTask:
		m.serveTask(w, r, spec)
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
	name, args, err := shell.Command(spec.Shell, spec.Command)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd := exec.CommandContext(ctx, name, args...)
	procutil.NoConsole(cmd)
	cmd.Stdin = r.Body
	cmd.Env = append(os.Environ(), "ASTREA_QUERY="+r.URL.RawQuery, "ASTREA_METHOD="+r.Method)
	cmd.WaitDelay = procutil.PipeGrace // don't hang on a detached child holding stdout
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		http.Error(w, "exec failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(out)
}

func (m *Manager) serveTask(w http.ResponseWriter, r *http.Request, spec *Spec) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	if m.Tasks == nil {
		fail(http.StatusServiceUnavailable, "tasks are not enabled on this agent")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	args := json.RawMessage(body)
	if len(bytes.TrimSpace(body)) == 0 {
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		args, _ = json.Marshal(q)
	}
	var out strings.Builder
	var mu sync.Mutex
	result, err := m.Tasks(r.Context(), spec.Task, args, func(c protocol.StreamChunk) {
		mu.Lock()
		if out.Len() < 1<<20 {
			out.WriteString(c.Data)
		}
		mu.Unlock()
	})
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "output": out.String()})
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
	// 0600: specs hold shell commands the agent will run.
	_ = os.WriteFile(m.store, data, 0o600)
}

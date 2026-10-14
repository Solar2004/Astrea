package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Restart policies.
const (
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"
	RestartNever     = "never"
)

// Service states.
const (
	SvcStarting = "starting"
	SvcRunning  = "running"
	SvcBackoff  = "backoff" // crashed, waiting to restart
	SvcExited   = "exited"  // finished and the policy says don't restart
	SvcStopped  = "stopped" // stopped on request
	SvcFailed   = "failed"  // could not be launched at all
)

// Ready describes how to tell that a service is up.
type Ready struct {
	// URL is polled until it answers 200 (llama-server's /health returns 503
	// while the model loads, so any-status would be too early).
	URL string `json:"url,omitempty"`
	// Port is dialled on 127.0.0.1 until it accepts a connection.
	Port int `json:"port,omitempty"`
}

// Expose publishes the service through a dynamic endpoint /dyn/<Endpoint>
// proxying to 127.0.0.1:<Port>. The supervisor only stores it; the caller
// wires the endpoint.
type Expose struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
}

// ServiceSpec is the persisted definition of a service.
type ServiceSpec struct {
	Name string `json:"name"`
	Command
	Restart   string  `json:"restart"`
	Autostart bool    `json:"autostart"`
	Ready     *Ready  `json:"ready,omitempty"`
	Expose    *Expose `json:"expose,omitempty"`
}

// ServiceStatus is a service's definition plus its live state.
type ServiceStatus struct {
	ServiceSpec
	State     string    `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Restarts  int       `json:"restarts"`
	LastExit  *int      `json:"last_exit,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	UptimeSec float64   `json:"uptime_sec,omitempty"`
	LogPath   string    `json:"log_path"`
}

type service struct {
	spec     ServiceSpec
	state    string
	proc     *process
	restarts int
	lastExit *int
	err      string
	stop     chan struct{}
	stopped  chan struct{} // closed when the run loop returns
}

// Services supervises long-lived processes and persists their definitions so
// autostart services come back when the agent restarts.
type Services struct {
	dir   string
	store string
	mu    sync.Mutex
	svcs  map[string]*service
}

// NewServices keeps logs and services.json under dir. Call StartAll to launch
// the persisted autostart services.
func NewServices(dir string) *Services {
	m := &Services{dir: dir, store: filepath.Join(dir, "services.json"), svcs: map[string]*service{}}
	m.load()
	return m
}

func (m *Services) logPath(name string) string { return filepath.Join(m.dir, name+".log") }

// Start defines (or redefines) a service and (re)starts it. If spec.Command
// is empty, the existing definition is restarted unchanged.
func (m *Services) Start(spec ServiceSpec) (ServiceStatus, error) {
	if err := ValidName(spec.Name); err != nil {
		return ServiceStatus{}, err
	}
	m.mu.Lock()
	old := m.svcs[spec.Name]
	m.mu.Unlock()
	if spec.Command.Command == "" {
		if old == nil {
			return ServiceStatus{}, fmt.Errorf("no service %q to restart; command is required to define it", spec.Name)
		}
		spec = old.spec
		spec.Autostart = true
	}
	switch spec.Restart {
	case "":
		spec.Restart = RestartOnFailure
	case RestartOnFailure, RestartAlways, RestartNever:
	default:
		return ServiceStatus{}, fmt.Errorf("restart must be on-failure, always or never")
	}
	if old != nil {
		m.halt(old)
	}
	s := &service{spec: spec, state: SvcStarting, stop: make(chan struct{}), stopped: make(chan struct{})}
	m.mu.Lock()
	m.svcs[spec.Name] = s
	m.mu.Unlock()
	m.save()
	go m.loop(s)
	return m.Status(spec.Name)
}

// halt stops s's run loop and waits for its process tree to die.
func (m *Services) halt(s *service) {
	if s.stop == nil {
		return // loaded from disk, never started
	}
	m.mu.Lock()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	m.mu.Unlock()
	<-s.stopped
}

func (m *Services) loop(s *service) {
	defer close(s.stopped)
	delay := time.Second
	for {
		p, err := spawn(s.spec.Command, m.logPath(s.spec.Name))
		m.mu.Lock()
		if err != nil {
			s.state, s.err, s.proc = SvcFailed, err.Error(), nil
		} else {
			s.state, s.err, s.proc = SvcRunning, "", p
		}
		m.mu.Unlock()

		if p != nil {
			select {
			case <-p.done:
			case <-s.stop:
				p.kill()
				m.mu.Lock()
				s.state = SvcStopped
				m.mu.Unlock()
				return
			}
			m.mu.Lock()
			code := p.exitCode
			s.lastExit = &code
			if p.err != nil {
				s.err = p.err.Error()
			}
			restart := s.spec.Restart == RestartAlways || (s.spec.Restart == RestartOnFailure && code != 0)
			if !restart {
				s.state = SvcExited
				m.mu.Unlock()
				return
			}
			s.state = SvcBackoff
			m.mu.Unlock()
			// A run that lasted a while was healthy: restart quickly again.
			if time.Since(p.started) > time.Minute {
				delay = time.Second
			}
		} else if s.spec.Restart == RestartNever {
			return
		}

		select {
		case <-s.stop:
			m.mu.Lock()
			s.state = SvcStopped
			m.mu.Unlock()
			return
		case <-time.After(delay):
		}
		m.mu.Lock()
		s.restarts++
		s.state = SvcStarting
		m.mu.Unlock()
		delay = min(delay*2, time.Minute)
	}
}

// Stop stops a service. With remove, its definition is deleted too; otherwise
// it is kept (autostart off) so it can be started again by name.
func (m *Services) Stop(name string, remove bool) (ServiceStatus, error) {
	m.mu.Lock()
	s, ok := m.svcs[name]
	m.mu.Unlock()
	if !ok {
		return ServiceStatus{}, fmt.Errorf("no service %q", name)
	}
	m.halt(s)
	st, _ := m.Status(name)
	m.mu.Lock()
	if remove {
		delete(m.svcs, name)
	} else {
		s.spec.Autostart = false
		st.Autostart = false
	}
	m.mu.Unlock()
	m.save()
	return st, nil
}

// Status returns one service's state.
func (m *Services) Status(name string) (ServiceStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.svcs[name]
	if !ok {
		return ServiceStatus{}, fmt.Errorf("no service %q", name)
	}
	return m.statusLocked(s), nil
}

func (m *Services) statusLocked(s *service) ServiceStatus {
	st := ServiceStatus{ServiceSpec: s.spec, State: s.state, Restarts: s.restarts,
		LastExit: s.lastExit, Error: s.err, LogPath: m.logPath(s.spec.Name)}
	if s.proc != nil && s.state == SvcRunning {
		st.PID = s.proc.cmd.Process.Pid
		st.StartedAt = s.proc.started
		st.UptimeSec = time.Since(s.proc.started).Round(time.Second).Seconds()
	}
	return st
}

// List returns every defined service, sorted by name.
func (m *Services) List() []ServiceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServiceStatus, 0, len(m.svcs))
	for _, s := range m.svcs {
		out = append(out, m.statusLocked(s))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Log reads a service's log (see ReadLog).
func (m *Services) Log(name string, offset int64, tailLines, maxBytes int) (LogChunk, error) {
	if err := ValidName(name); err != nil {
		return LogChunk{}, err
	}
	return ReadLog(m.logPath(name), offset, tailLines, maxBytes)
}

// WaitReady polls the service's Ready probe until it passes, the service
// dies, or timeout elapses. Without a probe it only checks the process is up.
func (m *Services) WaitReady(ctx context.Context, name string, timeout time.Duration) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		st, err := m.Status(name)
		if err != nil {
			return false, err.Error()
		}
		switch st.State {
		case SvcExited, SvcFailed, SvcStopped:
			return false, "service is " + st.State + " (see service.logs)"
		case SvcRunning:
			if probe(ctx, client, st.Ready) {
				return true, ""
			}
		}
		select {
		case <-ctx.Done():
			return false, "not ready yet after " + timeout.String() + "; keep polling service.status or read service.logs"
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func probe(ctx context.Context, client *http.Client, r *Ready) bool {
	switch {
	case r == nil:
		return true
	case r.URL != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	case r.Port > 0:
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.Port)))
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	return true
}

// StartAll launches every persisted autostart service (agent boot).
func (m *Services) StartAll() {
	m.mu.Lock()
	var specs []ServiceSpec
	for _, s := range m.svcs {
		if s.spec.Autostart {
			specs = append(specs, s.spec)
		}
	}
	m.mu.Unlock()
	for _, sp := range specs {
		_, _ = m.Start(sp)
	}
}

// StopAll stops every running service without changing its definition
// (agent shutdown), so autostart services come back next boot.
func (m *Services) StopAll() {
	m.mu.Lock()
	var all []*service
	for _, s := range m.svcs {
		all = append(all, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() { defer wg.Done(); m.halt(s) }()
	}
	wg.Wait()
}

func (m *Services) load() {
	b, err := os.ReadFile(m.store)
	if err != nil {
		return
	}
	var specs []ServiceSpec
	if json.Unmarshal(b, &specs) != nil {
		return
	}
	for _, sp := range specs {
		if ValidName(sp.Name) != nil {
			continue
		}
		// Not running yet: a loaded service has no run loop until started.
		s := &service{spec: sp, state: SvcStopped}
		m.svcs[sp.Name] = s
	}
}

func (m *Services) save() {
	m.mu.Lock()
	specs := make([]ServiceSpec, 0, len(m.svcs))
	for _, s := range m.svcs {
		specs = append(specs, s.spec)
	}
	m.mu.Unlock()
	sort.Slice(specs, func(a, b int) bool { return specs[a].Name < specs[b].Name })
	b, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(m.dir, 0o755)
	tmp := m.store + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.store)
	}
}

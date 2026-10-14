// Package fleet is the hub-side view of every controlled machine: which
// agents exist (machines.json), a reconnecting client per agent, and a
// parallel snapshot of their state (GPU, load, who is at the keyboard, which
// services run) so the AI — or the model gateway — can pick where work goes.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/transport"
)

// Machine is one agent. Same shape as the panel's config entries.
type Machine struct {
	Name  string `json:"name"`
	WS    string `json:"ws"`
	Token string `json:"token"`
}

type fileConfig struct {
	Machines []Machine `json:"machines"`
}

// DefaultPath is ~/.config/astrea/machines.json.
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "astrea", "machines.json")
	}
	return "machines.json"
}

// ErrNoConfig reports that no machines file exists.
var ErrNoConfig = errors.New("no machines file")

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// Load reads a machines file. If path is missing it falls back to the panel's
// panel.json next to it, so one file can describe the fleet for both.
func Load(path string) ([]Machine, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		alt := filepath.Join(filepath.Dir(path), "panel.json")
		if data, err = os.ReadFile(alt); errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoConfig
		}
		path = alt
	}
	if err != nil {
		return nil, err
	}
	var c fileConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := Validate(c.Machines); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c.Machines, nil
}

// Validate checks names are unique identifiers and URLs are ws(s)://.
func Validate(ms []Machine) error {
	if len(ms) == 0 {
		return errors.New("no machines listed")
	}
	seen := map[string]bool{}
	for _, m := range ms {
		if !nameRe.MatchString(m.Name) {
			return fmt.Errorf("machine name %q must match %s", m.Name, nameRe)
		}
		if seen[m.Name] {
			return fmt.Errorf("duplicate machine name %q", m.Name)
		}
		seen[m.Name] = true
		if _, err := BaseURL(m.WS); err != nil {
			return fmt.Errorf("machine %s: %w", m.Name, err)
		}
	}
	return nil
}

// BaseURL maps the agent's ws://host:port/ws to http://host:port.
func BaseURL(ws string) (string, error) {
	u, err := url.Parse(ws)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid ws url %q", ws)
	}
	switch u.Scheme {
	case "ws":
		return "http://" + u.Host, nil
	case "wss":
		return "https://" + u.Host, nil
	}
	return "", fmt.Errorf("ws url %q must start with ws:// or wss://", ws)
}

// Node is a machine plus its live client.
type Node struct {
	Machine
	Base   string // http://host:port
	Client *transport.ReconnectingClient
}

// CapsURL is the node's capability descriptor URL.
func (n *Node) CapsURL() string { return n.Base + "/capabilities" }

// Fleet is the set of nodes, in config order.
type Fleet struct {
	Nodes  []*Node
	byName map[string]*Node
}

// New builds clients for the machines (assumed validated).
func New(ms []Machine) *Fleet {
	f := &Fleet{byName: map[string]*Node{}}
	for _, m := range ms {
		base, _ := BaseURL(m.WS)
		n := &Node{Machine: m, Base: base, Client: transport.NewReconnecting(m.WS, m.Token)}
		f.Nodes = append(f.Nodes, n)
		f.byName[m.Name] = n
	}
	return f
}

// Get returns a node by name.
func (f *Fleet) Get(name string) *Node { return f.byName[name] }

// Names lists node names in config order.
func (f *Fleet) Names() []string {
	out := make([]string, len(f.Nodes))
	for i, n := range f.Nodes {
		out[i] = n.Name
	}
	return out
}

// Each runs fn for every node in parallel and waits.
func (f *Fleet) Each(fn func(n *Node)) {
	var wg sync.WaitGroup
	for _, n := range f.Nodes {
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
			fn(n)
		}(n)
	}
	wg.Wait()
}

// --- status ---

// GPU is the per-GPU slice of sys.stats the fleet view keeps.
type GPU struct {
	Name         string   `json:"name"`
	VRAMTotalMiB uint64   `json:"vram_total_mib"`
	VRAMFreeMiB  *uint64  `json:"vram_free_mib,omitempty"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
}

// Service is a supervised service on a node.
type Service struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint,omitempty"`
}

// Status is one node's snapshot.
type Status struct {
	Name       string    `json:"name"`
	Online     bool      `json:"online"`
	Error      string    `json:"error,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
	OS         string    `json:"os,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	Locked     bool      `json:"locked,omitempty"`
	CPUPercent float64   `json:"cpu_percent"`
	RAMPercent float64   `json:"ram_percent"`
	RAMFreeGiB float64   `json:"ram_free_gib"`
	GPUs       []GPU     `json:"gpus,omitempty"`
	UserActive *bool     `json:"user_active,omitempty"`
	IdleSec    *float64  `json:"idle_sec,omitempty"`
	Foreground string    `json:"foreground_process,omitempty"`
	Services   []Service `json:"services,omitempty"`
}

// StatusAll snapshots every node in parallel. Offline nodes are reported, not
// skipped.
func (f *Fleet) StatusAll(ctx context.Context) []Status {
	out := make([]Status, len(f.Nodes))
	idx := map[*Node]int{}
	for i, n := range f.Nodes {
		idx[n] = i
	}
	f.Each(func(n *Node) { out[idx[n]] = n.Status(ctx) })
	return out
}

// Status snapshots one node: admin.status, sys.stats, sys.idle, service.status.
func (n *Node) Status(ctx context.Context) Status {
	st := Status{Name: n.Name}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var admin struct {
		OS     string `json:"os"`
		Mode   string `json:"mode"`
		Locked bool   `json:"locked"`
	}
	if err := n.Call(ctx, "admin.status", &admin); err != nil {
		st.Error = err.Error()
		return st
	}
	st.Online, st.OS, st.Mode, st.Locked = true, admin.OS, admin.Mode, admin.Locked

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string
	run := func(method string, into any, apply func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := n.Call(ctx, method, into)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, method+": "+err.Error())
				return
			}
			apply()
		}()
	}

	var stats struct {
		Host struct {
			Hostname string `json:"hostname"`
		} `json:"host"`
		CPU struct {
			Usage float64 `json:"usage_percent"`
		} `json:"cpu"`
		Memory struct {
			Available   uint64  `json:"available"`
			UsedPercent float64 `json:"used_percent"`
		} `json:"memory"`
		GPUs []struct {
			Name      string   `json:"name"`
			VRAMTotal uint64   `json:"vram_total"`
			VRAMUsed  *uint64  `json:"vram_used"`
			Usage     *float64 `json:"usage_percent"`
		} `json:"gpus"`
	}
	run("sys.stats", &stats, func() {
		st.Hostname = stats.Host.Hostname
		st.CPUPercent = round1(stats.CPU.Usage)
		st.RAMPercent = round1(stats.Memory.UsedPercent)
		st.RAMFreeGiB = round1(float64(stats.Memory.Available) / (1 << 30))
		for _, g := range stats.GPUs {
			gg := GPU{Name: g.Name, VRAMTotalMiB: g.VRAMTotal >> 20, UsagePercent: g.Usage}
			if g.VRAMUsed != nil && *g.VRAMUsed <= g.VRAMTotal {
				free := (g.VRAMTotal - *g.VRAMUsed) >> 20
				gg.VRAMFreeMiB = &free
			}
			st.GPUs = append(st.GPUs, gg)
		}
	})

	var idle struct {
		IdleSec    float64 `json:"idle_sec"`
		Process    string  `json:"process"`
		UserActive bool    `json:"user_active"`
	}
	run("sys.idle", &idle, func() {
		st.IdleSec, st.UserActive, st.Foreground = &idle.IdleSec, &idle.UserActive, idle.Process
	})

	var svcs struct {
		Services []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Expose *struct {
				Endpoint string `json:"endpoint"`
			} `json:"expose"`
		} `json:"services"`
	}
	run("service.status", &svcs, func() {
		for _, s := range svcs.Services {
			sv := Service{Name: s.Name, State: s.State}
			if s.Expose != nil {
				sv.Endpoint = s.Expose.Endpoint
			}
			st.Services = append(st.Services, sv)
		}
	})
	wg.Wait()
	// Missing optional capabilities (older agents, no xprintidle) are not
	// fatal; only report them when nothing else came back.
	if st.Hostname == "" && len(errs) > 0 {
		st.Error = strings.Join(errs, "; ")
	}
	return st
}

// Call invokes method with no arguments and decodes the result into out.
func (n *Node) Call(ctx context.Context, method string, out any) error {
	res, err := n.Client.Call(ctx, method, map[string]any{})
	if err != nil {
		return err
	}
	return json.Unmarshal(res.Result, out)
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

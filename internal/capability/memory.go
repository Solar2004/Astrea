package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/deps"
	"github.com/Solar2004/Astrea/internal/memory"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/work"
)

// Inventory is what is on this PC, refreshed every few hours.
type Inventory struct {
	Updated  time.Time `json:"updated"`
	OS       string    `json:"os"`
	CPU      string    `json:"cpu,omitempty"`
	RAMGB    float64   `json:"ram_gb"`
	GPUs     []string  `json:"gpus,omitempty"`
	Disks    []string  `json:"disks,omitempty"`
	Tools    []string  `json:"tools,omitempty"`   // "python 3.12.4"
	Missing  []string  `json:"missing,omitempty"` // known tools not installed
	Services []string  `json:"services,omitempty"`
	Programs []string  `json:"programs,omitempty"` // installed programs (Windows)
}

const inventoryTTL = 6 * time.Hour

// Memory serves memory.* from the notes, an inventory built by asking the
// registry's own capabilities, and the recent tasks.
type Memory struct {
	Notes *memory.Store
	Works *work.Store
	Reg   *Registry

	mu  sync.Mutex
	inv *Inventory
}

// RegisterMemory registers memory.get, memory.note and memory.forget, and
// builds the first inventory in the background.
func RegisterMemory(r *Registry, m *Memory) {
	r.Register(MemoryGet{m})
	r.Register(MemoryNote{m})
	r.Register(MemoryForget{m})
	go m.Inventory(context.Background(), false)
}

// Inventory returns the cached inventory, rebuilding it when stale or asked.
func (m *Memory) Inventory(ctx context.Context, refresh bool) Inventory {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inv != nil && !refresh && time.Since(m.inv.Updated) < inventoryTTL {
		return *m.inv
	}
	inv := m.build(ctx)
	m.inv = &inv
	return inv
}

func (m *Memory) call(ctx context.Context, method string, out any) {
	res, err := m.Reg.Dispatch(ctx, method, json.RawMessage(`{}`), func(protocol.StreamChunk) {})
	if err != nil {
		return
	}
	if data, err := json.Marshal(res); err == nil {
		_ = json.Unmarshal(data, out)
	}
}

func gb(b uint64) float64 { return float64(b*10/(1<<30)) / 10 }

func (m *Memory) build(ctx context.Context) Inventory {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	inv := Inventory{Updated: time.Now()}
	var st struct {
		Host struct{ Platform, PlatformVersion string } `json:"host"`
		CPU  struct {
			Model   string `json:"model"`
			Threads int    `json:"threads"`
		} `json:"cpu"`
		Memory struct{ Total uint64 } `json:"memory"`
		GPUs   []struct {
			Name      string `json:"name"`
			VRAMTotal uint64 `json:"vram_total"`
			Shared    bool   `json:"shared_memory"`
		} `json:"gpus"`
		Disks []struct {
			Mount       string `json:"mount"`
			Total, Used uint64
		} `json:"disks"`
	}
	m.call(ctx, "sys.stats", &st)
	var info struct {
		Platform        string `json:"platform"`
		PlatformVersion string `json:"platform_version"`
		MemTotal        uint64 `json:"mem_total"`
	}
	m.call(ctx, "sys.info", &info)
	inv.OS = strings.TrimSpace(info.Platform + " " + info.PlatformVersion)
	inv.RAMGB = gb(max(st.Memory.Total, info.MemTotal))
	if st.CPU.Model != "" {
		inv.CPU = fmt.Sprintf("%s (%d hilos)", strings.TrimSpace(st.CPU.Model), st.CPU.Threads)
	}
	for _, g := range st.GPUs {
		s := g.Name
		if g.VRAMTotal > 0 && !g.Shared {
			s += fmt.Sprintf(" %.0f GB", gb(g.VRAMTotal))
		}
		inv.GPUs = append(inv.GPUs, s)
	}
	for _, d := range st.Disks {
		inv.Disks = append(inv.Disks, fmt.Sprintf("%s %.0f/%.0f GB libres", d.Mount, gb(d.Total-d.Used), gb(d.Total)))
	}
	deps.RefreshPath()
	for _, name := range deps.Names() {
		d, err := deps.Lookup(name)
		if err != nil {
			continue
		}
		s := deps.Check(ctx, d)
		switch {
		case s.Present && versionRE.MatchString(s.Version):
			inv.Tools = append(inv.Tools, name+" "+versionRE.FindString(s.Version))
		case s.Present:
			inv.Tools = append(inv.Tools, name)
		default:
			inv.Missing = append(inv.Missing, name)
		}
	}
	var svc struct {
		Services []struct{ Name, State string } `json:"services"`
	}
	m.call(ctx, "service.status", &svc)
	for _, s := range svc.Services {
		inv.Services = append(inv.Services, s.Name+" ("+s.State+")")
	}
	inv.Programs = installedPrograms()
	return inv
}

// versionRE picks "4.4.2" out of "cmake version 4.4.2 …".
var versionRE = regexp.MustCompile(`\d+(\.\d+)+`)

// Summary is the compact text handed to the AI when it starts a task here.
func (m *Memory) Summary(ctx context.Context) string {
	inv := m.Inventory(ctx, false)
	var b strings.Builder
	line := func(label string, items []string, max int) {
		if len(items) == 0 {
			return
		}
		more := ""
		if len(items) > max {
			more = fmt.Sprintf(" … (+%d)", len(items)-max)
			items = items[:max]
		}
		fmt.Fprintf(&b, "%s: %s%s\n", label, strings.Join(items, ", "), more)
	}
	fmt.Fprintf(&b, "PC: %s · %.0f GB RAM", inv.OS, inv.RAMGB)
	if inv.CPU != "" {
		b.WriteString(" · " + inv.CPU)
	}
	b.WriteString("\n")
	line("GPU", inv.GPUs, 4)
	line("Disks", inv.Disks, 6)
	line("Tools", inv.Tools, 30)
	line("Not installed", inv.Missing, 30)
	line("Services", inv.Services, 20)
	line("Programs", inv.Programs, 60)
	if notes := m.Notes.Notes(); len(notes) > 0 {
		b.WriteString("Notes (memory.note to add, memory.forget to drop a wrong one):\n")
		for i := len(notes) - 1; i >= 0 && i >= len(notes)-40; i-- {
			n := notes[i]
			fmt.Fprintf(&b, "- [%s] %s", n.ID, n.Text)
			if len(n.Tags) > 0 {
				b.WriteString(" #" + strings.Join(n.Tags, " #"))
			}
			b.WriteString("\n")
		}
	}
	if m.Works != nil {
		if tasks := m.Works.List(8); len(tasks) > 0 {
			b.WriteString("Recent tasks here:\n")
			for _, t := range tasks {
				if t.Status == "running" {
					continue // the one just opened
				}
				fmt.Fprintf(&b, "- %s %s: %s", t.Started.Format("2006-01-02"), t.Status, t.Title)
				if t.Summary != "" {
					b.WriteString(" — " + clipStr(t.Summary, 200))
				}
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func clipStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// --- capabilities ---

type MemoryGet struct{ M *Memory }

func (MemoryGet) Name() string { return "memory.get" }

func (MemoryGet) Describe() Descriptor {
	return Descriptor{
		Name: "memory.get",
		Description: "What this PC has and what was done here: hardware, installed tools and programs, services, notes left by you or the owner " +
			"(quirks, where things live) and the recent tasks. It is handed to you automatically when you open a task on a PC. refresh re-scans the inventory.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"refresh": {"type": "boolean"}}}`),
	}
}

func (c MemoryGet) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Refresh bool }
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	inv := c.M.Inventory(ctx, req.Refresh)
	return map[string]any{
		"summary":   c.M.Summary(ctx),
		"inventory": inv,
		"notes":     c.M.Notes.Notes(),
	}, nil
}

type MemoryNote struct{ M *Memory }

func (MemoryNote) Name() string { return "memory.note" }

func (MemoryNote) Describe() Descriptor {
	return Descriptor{
		Name: "memory.note",
		Description: "Remember something about this PC for next time: where a program or model lives, how to start it, a quirk or a fix that worked " +
			"(e.g. \"ComfyUI: D:\\ComfyUI, start with run_nvidia_gpu.bat\", \"Python 3.10 needed for project X\"). Short, one fact per note.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"text": {"type": "string"},
				"tags": {"type": "array", "items": {"type": "string"}}
			},
			"required": ["text"]
		}`),
	}
}

func (c MemoryNote) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Text string   `json:"text"`
		Tags []string `json:"tags"`
		From string   `json:"from"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	n, err := c.M.Notes.Add(req.Text, req.Tags, req.From)
	if err != nil {
		return nil, badRequest(err)
	}
	return n, nil
}

type MemoryForget struct{ M *Memory }

func (MemoryForget) Name() string { return "memory.forget" }

func (MemoryForget) Describe() Descriptor {
	return Descriptor{
		Name:        "memory.forget",
		Description: "Delete a note about this PC that is wrong or outdated (id from memory.get).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]}`),
	}
}

func (c MemoryForget) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ ID string }
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if err := c.M.Notes.Forget(req.ID); err != nil {
		return nil, badRequest(err)
	}
	return map[string]any{"forgotten": req.ID}, nil
}

// sortedUnique sorts names and drops duplicates and blanks.
func sortedUnique(names []string) []string {
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	out := names[:0]
	for i, n := range names {
		if n != "" && (i == 0 || !strings.EqualFold(n, names[i-1])) {
			out = append(out, n)
		}
	}
	return out
}

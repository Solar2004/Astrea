package capability

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// SysInfo reports host, CPU and memory information about the controlled machine.
type SysInfo struct{}

func (SysInfo) Name() string { return "sys.info" }

func (SysInfo) Describe() Descriptor {
	return Descriptor{
		Name:        "sys.info",
		Description: "Report host OS, platform, uptime and memory of the controlled machine.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (SysInfo) Invoke(ctx context.Context, _ json.RawMessage, _ Emit) (any, error) {
	h, err := host.InfoWithContext(ctx)
	if err != nil {
		return nil, internal(err)
	}
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{
		"hostname":         h.Hostname,
		"os":               h.OS,
		"platform":         h.Platform,
		"platform_version": h.PlatformVersion,
		"kernel_arch":      h.KernelArch,
		"uptime_sec":       h.Uptime,
		"mem_total":        vm.Total,
		"mem_used":         vm.Used,
		"mem_used_percent": vm.UsedPercent,
	}, nil
}

// ProcList lists running processes, optionally filtered by a name substring.
type ProcList struct{}

func (ProcList) Name() string { return "proc.list" }

func (ProcList) Describe() Descriptor {
	return Descriptor{
		Name:        "proc.list",
		Description: "List running processes on the controlled machine. Optional name filter (case-insensitive substring).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filter": {"type": "string", "description": "Only include processes whose name contains this substring."},
				"limit": {"type": "integer", "description": "Max processes to return (default 200)."}
			}
		}`),
	}
}

type procInfo struct {
	PID  int32   `json:"pid"`
	Name string  `json:"name"`
	CPU  float64 `json:"cpu_percent"`
	Mem  float32 `json:"mem_percent"`
}

func (ProcList) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Filter string `json:"filter"`
		Limit  int    `json:"limit"`
	}
	_ = json.Unmarshal(payload, &req)
	if req.Limit <= 0 {
		req.Limit = 200
	}
	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, internal(err)
	}
	filter := strings.ToLower(req.Filter)
	out := make([]procInfo, 0, len(procs))
	for _, p := range procs {
		name, _ := p.NameWithContext(ctx)
		if filter != "" && !strings.Contains(strings.ToLower(name), filter) {
			continue
		}
		cpu, _ := p.CPUPercentWithContext(ctx)
		memPct, _ := p.MemoryPercentWithContext(ctx)
		out = append(out, procInfo{PID: p.Pid, Name: name, CPU: cpu, Mem: memPct})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPU > out[j].CPU })
	if len(out) > req.Limit {
		out = out[:req.Limit]
	}
	return map[string]any{"count": len(out), "processes": out}, nil
}

// ProcKill terminates a process by PID.
type ProcKill struct{}

func (ProcKill) Name() string { return "proc.kill" }

func (ProcKill) Describe() Descriptor {
	return Descriptor{
		Name:        "proc.kill",
		Description: "Terminate a process by PID on the controlled machine.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"pid": {"type": "integer"}},
			"required": ["pid"]
		}`),
	}
}

func (ProcKill) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		PID int32 `json:"pid"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.PID == 0 {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "pid is required"}
	}
	p, err := process.NewProcessWithContext(ctx, req.PID)
	if err != nil {
		return nil, internal(err)
	}
	if err := p.TerminateWithContext(ctx); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"pid": req.PID, "terminated": true}, nil
}

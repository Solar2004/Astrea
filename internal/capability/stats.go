package capability

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// SysStats reports hardware specs (CPU, RAM, GPU, disks) together with their
// live usage. It is cheap enough to poll every couple of seconds, which is what
// astrea-panel does. Static facts (CPU model, GPU list) are cached.
type SysStats struct {
	once    sync.Once
	cpuInfo cpuStatic
}

type cpuStatic struct {
	Model   string  `json:"model"`
	Cores   int     `json:"cores"`
	Threads int     `json:"threads"`
	MHz     float64 `json:"mhz"`
}

type cpuStats struct {
	cpuStatic
	UsagePercent float64   `json:"usage_percent"`
	PerCore      []float64 `json:"per_core"`
}

type memStats struct {
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	Available   uint64  `json:"available"`
	UsedPercent float64 `json:"used_percent"`
	SwapTotal   uint64  `json:"swap_total"`
	SwapUsed    uint64  `json:"swap_used"`
}

type diskStats struct {
	Mount       string  `json:"mount"`
	FSType      string  `json:"fstype"`
	Total       uint64  `json:"total"`
	Used        uint64  `json:"used"`
	UsedPercent float64 `json:"used_percent"`
}

type hostStats struct {
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Platform        string `json:"platform"`
	PlatformVersion string `json:"platform_version"`
	KernelArch      string `json:"kernel_arch"`
	UptimeSec       uint64 `json:"uptime_sec"`
}

type statsResult struct {
	Time   time.Time   `json:"time"`
	Host   hostStats   `json:"host"`
	CPU    cpuStats    `json:"cpu"`
	Memory memStats    `json:"memory"`
	GPUs   []GPU       `json:"gpus"`
	Disks  []diskStats `json:"disks"`
}

func (*SysStats) Name() string { return "sys.stats" }

func (*SysStats) Describe() Descriptor {
	return Descriptor{
		Name:        "sys.stats",
		Description: "Hardware specs and live usage of the controlled machine: CPU model/cores/usage (total and per core), RAM and swap, GPUs (name, VRAM, usage, temperature when available) and disks.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (s *SysStats) Invoke(ctx context.Context, _ json.RawMessage, _ Emit) (any, error) {
	s.once.Do(func() { s.cpuInfo = readCPUStatic(ctx) })

	res := statsResult{Time: time.Now().UTC(), CPU: cpuStats{cpuStatic: s.cpuInfo}}

	if h, err := host.InfoWithContext(ctx); err == nil {
		res.Host = hostStats{h.Hostname, h.OS, h.Platform, h.PlatformVersion, h.KernelArch, h.Uptime}
	}

	// Interval 0 measures since the previous call, so successive polls give
	// the usage over the polling period without blocking.
	if p, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(p) > 0 {
		res.CPU.UsagePercent = p[0]
	}
	if p, err := cpu.PercentWithContext(ctx, 0, true); err == nil {
		res.CPU.PerCore = p
	}

	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return nil, internal(err)
	}
	res.Memory = memStats{Total: vm.Total, Used: vm.Used, Available: vm.Available, UsedPercent: vm.UsedPercent}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		res.Memory.SwapTotal, res.Memory.SwapUsed = sw.Total, sw.Used
	}

	res.GPUs = gpuStats(ctx)
	if res.GPUs == nil {
		res.GPUs = []GPU{}
	}
	res.Disks = readDisks(ctx)
	return res, nil
}

func readCPUStatic(ctx context.Context) cpuStatic {
	var st cpuStatic
	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		st.Model = infos[0].ModelName
		st.MHz = infos[0].Mhz
	}
	if n, err := cpu.CountsWithContext(ctx, false); err == nil {
		st.Cores = n
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil {
		st.Threads = n
	}
	return st
}

func readDisks(ctx context.Context) []diskStats {
	out := []diskStats{}
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return out
	}
	seen := map[string]bool{} // by device: btrfs subvolumes share one
	for _, p := range parts {
		if seen[p.Device] {
			continue
		}
		seen[p.Device] = true
		// Empty card readers / DVD drives fail here; skip them.
		u, err := disk.UsageWithContext(ctx, p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		out = append(out, diskStats{Mount: p.Mountpoint, FSType: p.Fstype, Total: u.Total, Used: u.Used, UsedPercent: u.UsedPercent})
	}
	return out
}

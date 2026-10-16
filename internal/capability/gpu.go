package capability

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
)

// GPU is one graphics adapter. Usage, VRAM in use and temperature are pointers
// because not every driver exposes them; nil means "unknown", not zero.
type GPU struct {
	Name         string   `json:"name"`
	Vendor       string   `json:"vendor"`
	VRAMTotal    uint64   `json:"vram_total"`
	VRAMUsed     *uint64  `json:"vram_used,omitempty"`
	UsagePercent *float64 `json:"usage_percent,omitempty"`
	TemperatureC *float64 `json:"temperature_c,omitempty"`
	// SharedMemory marks integrated GPUs whose "VRAM" is borrowed system RAM.
	SharedMemory bool `json:"shared_memory,omitempty"`
}

func vendorName(id uint32) string {
	switch id {
	case 0x10de:
		return "NVIDIA"
	case 0x1002, 0x1022:
		return "AMD"
	case 0x8086:
		return "Intel"
	case 0x5143:
		return "Qualcomm"
	case 0x1414:
		return "Microsoft"
	}
	return ""
}

// nvidiaGPU is one row of nvidia-smi output.
type nvidiaGPU struct {
	Name      string
	VRAMTotal uint64 // bytes
	VRAMUsed  uint64 // bytes
	Usage     *float64
	Temp      *float64
}

// nvidiaSMI queries NVIDIA GPUs through nvidia-smi, which ships with the
// driver on both Windows and Linux. It returns nil if the tool is missing.
func nvidiaSMI(ctx context.Context, paths ...string) []nvidiaGPU {
	bin := ""
	for _, p := range append([]string{"nvidia-smi"}, paths...) {
		if found, err := exec.LookPath(p); err == nil {
			bin = found
			break
		}
	}
	if bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"--query-gpu=name,memory.total,memory.used,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits")
	procutil.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}

// parseNvidiaSMI parses `--format=csv,noheader,nounits` rows of
// name, memory.total (MiB), memory.used (MiB), utilization.gpu (%), temperature.gpu (C).
// Unsupported fields come back as "[N/A]" or "[Not Supported]".
func parseNvidiaSMI(out string) []nvidiaGPU {
	var gpus []nvidiaGPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := nvidiaGPU{Name: f[0], Usage: parseFloat(f[3]), Temp: parseFloat(f[4])}
		if v := parseFloat(f[1]); v != nil {
			g.VRAMTotal = uint64(*v) << 20
		}
		if v := parseFloat(f[2]); v != nil {
			g.VRAMUsed = uint64(*v) << 20
		}
		gpus = append(gpus, g)
	}
	return gpus
}

func parseFloat(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// mergeNvidia fills in live data from nvidia-smi for the NVIDIA adapters in
// gpus, matching by name (each nvidia-smi row is used once).
func mergeNvidia(gpus []GPU, nv []nvidiaGPU) {
	used := make([]bool, len(nv))
	for i := range gpus {
		if gpus[i].Vendor != "NVIDIA" {
			continue
		}
		for j, n := range nv {
			if used[j] || !strings.EqualFold(strings.TrimSpace(gpus[i].Name), n.Name) {
				continue
			}
			used[j] = true
			applyNvidia(&gpus[i], n)
			break
		}
	}
}

func applyNvidia(g *GPU, n nvidiaGPU) {
	if n.VRAMTotal > 0 {
		g.VRAMTotal = n.VRAMTotal
		v := n.VRAMUsed
		g.VRAMUsed = &v
	}
	if n.Usage != nil {
		g.UsagePercent = n.Usage
	}
	if n.Temp != nil {
		g.TemperatureC = n.Temp
	}
}

// Windows GPU performance-counter instance names look like
//
//	pid_1234_luid_0x00000000_0x0000C6F2_phys_0_eng_3_engtype_3D   (GPU Engine)
//	luid_0x00000000_0x0000C6F2_phys_0                              (GPU Adapter Memory)
//
// The LUID ties a counter to a DXGI adapter. They live here, not in the
// Windows file, so the parsing is tested on every platform.
var (
	rePID    = regexp.MustCompile(`^pid_(\d+)_`)
	reLUID   = regexp.MustCompile(`luid_0x([0-9A-Fa-f]{8})_0x([0-9A-Fa-f]{8})`)
	reEngine = regexp.MustCompile(`_eng_(\d+)`)
)

// luidKey normalizes a LUID as "HIGH_LOW" in upper-case hex.
func luidKey(high, low uint32) string {
	return fmt.Sprintf("%08X_%08X", high, low)
}

// luidFromInstance extracts the luidKey from a counter instance name.
func luidFromInstance(name string) (string, bool) {
	m := reLUID.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return strings.ToUpper(m[1] + "_" + m[2]), true
}

// gpuUtilization reproduces Task Manager's per-adapter GPU %: per engine,
// sum the utilization of every process using it; the adapter's figure is its
// busiest engine. Input maps engine instance name -> utilization percent.
func gpuUtilization(engines map[string]float64) map[string]float64 {
	perEngine := map[string]float64{} // luid + engine index -> summed %
	for name, v := range engines {
		luid, ok := luidFromInstance(name)
		if !ok {
			continue
		}
		eng := "?"
		if m := reEngine.FindStringSubmatch(name); m != nil {
			eng = m[1]
		}
		perEngine[luid+"|"+eng] += v
	}
	out := map[string]float64{}
	for k, v := range perEngine {
		luid := k[:strings.IndexByte(k, '|')]
		if v > 100 {
			v = 100
		}
		if v > out[luid] {
			out[luid] = v
		}
	}
	return out
}

// ProcGPU is one process's use of the GPUs.
type ProcGPU struct {
	Mem     uint64   // dedicated VRAM, bytes
	Percent *float64 // busiest engine, Windows only
}

// procGPUs folds the per-process counter instances ("pid_1234_luid_…") by
// pid: memory summed over adapters, utilization as gpuUtilization per process.
func procGPUs(engines, mem map[string]float64) map[int]ProcGPU {
	out := map[int]ProcGPU{}
	byPID := map[int]map[string]float64{}
	for name, v := range engines {
		if m := rePID.FindStringSubmatch(name); m != nil {
			pid, _ := strconv.Atoi(m[1])
			if byPID[pid] == nil {
				byPID[pid] = map[string]float64{}
			}
			byPID[pid][name] = v
		}
	}
	for pid, e := range byPID {
		best := 0.0
		for _, v := range gpuUtilization(e) {
			best = max(best, v)
		}
		if best > 0 {
			p := out[pid]
			p.Percent = &best
			out[pid] = p
		}
	}
	for name, v := range mem {
		if m := rePID.FindStringSubmatch(name); m != nil && v > 0 {
			pid, _ := strconv.Atoi(m[1])
			p := out[pid]
			p.Mem += uint64(v)
			out[pid] = p
		}
	}
	return out
}

// nvidiaProcs reads the VRAM of each process using an NVIDIA GPU (nil when
// nvidia-smi is missing; Windows reports [N/A] here, so it uses counters).
func nvidiaProcs(ctx context.Context) map[int]ProcGPU {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	procutil.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseComputeApps(string(out))
}

// parseComputeApps parses "pid, used_memory (MiB)" rows.
func parseComputeApps(out string) map[int]ProcGPU {
	res := map[int]ProcGPU{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		if v := parseFloat(strings.TrimSpace(f[1])); v != nil {
			p := res[pid]
			p.Mem += uint64(*v) << 20
			res[pid] = p
		}
	}
	return res
}

//go:build linux

package capability

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GPUs on Linux come from /sys/class/drm: amdgpu exposes usage and VRAM
// there, i915/xe only identity. NVIDIA's proprietary driver exposes nothing
// in sysfs, so those cards are filled in from nvidia-smi.
func gpuStats(ctx context.Context) []GPU {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*")
	var out []GPU
	seen := map[string]bool{}
	for _, card := range cards {
		if strings.Contains(filepath.Base(card), "-") {
			continue // connectors like card0-HDMI-A-1
		}
		dev := filepath.Join(card, "device")
		devPath, err := filepath.EvalSymlinks(dev)
		if err != nil || seen[devPath] {
			continue
		}
		seen[devPath] = true
		vendor := readHex(filepath.Join(dev, "vendor"))
		if vendor == 0 {
			continue
		}
		g := GPU{Vendor: vendorName(uint32(vendor)), Name: pciName(ctx, filepath.Base(devPath))}
		if g.Name != "" && g.Vendor != "" && !strings.HasPrefix(g.Name, g.Vendor) {
			g.Name = g.Vendor + " " + g.Name
		}
		if g.Name == "" {
			g.Name = strings.TrimSpace(g.Vendor + " GPU " + strconv.FormatUint(readHex(filepath.Join(dev, "device")), 16))
		}
		if v, ok := readUint(filepath.Join(dev, "mem_info_vram_total")); ok {
			g.VRAMTotal = v
		}
		if v, ok := readUint(filepath.Join(dev, "mem_info_vram_used")); ok {
			g.VRAMUsed = &v
		}
		if v, ok := readUint(filepath.Join(dev, "gpu_busy_percent")); ok {
			f := float64(v)
			g.UsagePercent = &f
		}
		if temps, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*", "temp1_input")); len(temps) > 0 {
			if v, ok := readUint(temps[0]); ok {
				c := float64(v) / 1000
				g.TemperatureC = &c
			}
		}
		out = append(out, g)
	}

	nv := nvidiaSMI(ctx)
	if len(nv) == 0 {
		return out
	}
	// sysfs names come from lspci and rarely match nvidia-smi's, so pair the
	// NVIDIA cards in order and take nvidia-smi's (better) name.
	i := 0
	for k := range out {
		if out[k].Vendor == "NVIDIA" && i < len(nv) {
			out[k].Name = nv[i].Name
			applyNvidia(&out[k], nv[i])
			i++
		}
	}
	for ; i < len(nv); i++ {
		g := GPU{Name: nv[i].Name, Vendor: "NVIDIA"}
		applyNvidia(&g, nv[i])
		out = append(out, g)
	}
	return out
}

// pciName asks lspci for the device's marketing name; "" if unavailable.
func pciName(ctx context.Context, slot string) string {
	if _, err := exec.LookPath("lspci"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lspci", "-s", slot).Output()
	if err != nil {
		return ""
	}
	// "03:00.0 VGA compatible controller: Advanced Micro Devices, Inc. [AMD/ATI] Navi 22 [Radeon RX 6700 XT] (rev c5)"
	line := strings.TrimSpace(string(out))
	if i := strings.Index(line, ": "); i >= 0 {
		line = line[i+2:]
	}
	if i := strings.LastIndex(line, " (rev "); i >= 0 {
		line = line[:i]
	}
	// "... Polaris 20 XL [Radeon RX 580 2048SP]" -> "Radeon RX 580 2048SP"
	if strings.HasSuffix(line, "]") {
		if i := strings.LastIndex(line, "["); i >= 0 {
			return line[i+1 : len(line)-1]
		}
	}
	return line
}

func readUint(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
}

func readHex(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(b)), "0x"), 16, 32)
	return v
}

func gpuProcs(ctx context.Context) map[int]ProcGPU { return nvidiaProcs(ctx) }

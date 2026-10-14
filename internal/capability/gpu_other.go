//go:build !windows && !linux

package capability

import "context"

func gpuStats(ctx context.Context) []GPU {
	var out []GPU
	for _, n := range nvidiaSMI(ctx) {
		g := GPU{Name: n.Name, Vendor: "NVIDIA"}
		applyNvidia(&g, n)
		out = append(out, g)
	}
	return out
}

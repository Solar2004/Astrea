package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Set ASTREA_CARD_DIR to write the PNGs somewhere and look at them.
func TestRenderCards(t *testing.T) {
	stats := json.RawMessage(`{"host":{"hostname":"f3-1","platform":"Microsoft Windows 11 Pro","platform_version":"25H2","uptime_sec":26000},
	 "cpu":{"model":"Intel(R) Core(TM) Ultra 7 265","usage_percent":23,"cores":20,"threads":20},
	 "memory":{"total":68000000000,"used":11400000000,"used_percent":16.8},
	 "gpus":[{"name":"Intel(R) Graphics","vendor":"Intel","vram_used":91000000,"vram_total":50900000000,"shared_memory":true},
	         {"name":"NVIDIA RTX 2000 Ada Generation","vendor":"NVIDIA","usage_percent":64,"vram_used":9800000000,"vram_total":17100000000,"temperature_c":61}],
	 "disks":[{"mount":"C:","total":988000000000,"used":95000000000,"used_percent":9.6}]}`)
	on := machineStatus{Name: "f3-1", Online: true, Stats: stats,
		Idle:     json.RawMessage(`{"session":"active","user_active":false,"idle_sec":3900}`),
		Admin:    json.RawMessage(`{"locked":false,"mode":"yolo"}`),
		Services: json.RawMessage(`{"services":[{"name":"llama","state":"running"},{"name":"comfy","state":"backoff"}]}`)}
	off := machineStatus{Name: "pc-aula", LastSeen: time.Now().Add(-47 * time.Minute).UnixMilli()}
	busy := machineStatus{Name: "pc-aula2", Online: true, Stats: stats, Idle: json.RawMessage(`{"session":"active","user_active":true,"process":"Roblox.exe"}`)}

	h := cardHistory{Step: 15 * time.Second, End: time.Now()}
	for i := 0; i < 300; i++ {
		v := 30 + 25*math.Sin(float64(i)/18) + 10*math.Sin(float64(i)/3)
		if i > 120 && i < 140 {
			v = -1 // gap: machine off
		}
		h.CPU = append(h.CPU, v)
		h.RAM = append(h.RAM, math.Max(-1, 16+float64(i)/30))
		h.GPU = append(h.GPU, math.Max(-1, 64*math.Abs(math.Sin(float64(i)/40))))
	}

	cases := map[string]func() ([]byte, error){
		"machine": func() ([]byte, error) { return renderMachineCard(on, h) },
		"machine-new": func() ([]byte, error) {
			return renderMachineCard(on, cardHistory{Step: 15 * time.Second, CPU: []float64{12}})
		},
		"machine-off": func() ([]byte, error) { return renderMachineCard(off, h) },
		"fleet":       func() ([]byte, error) { return renderFleetCard([]machineStatus{on, busy, off}) },
		"fleet-empty": func() ([]byte, error) { return renderFleetCard(nil) },
	}
	for name, fn := range cases {
		b, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := png.Decode(bytes.NewReader(b)); err != nil {
			t.Fatalf("%s: invalid PNG: %v", name, err)
		}
		if dir := os.Getenv("ASTREA_CARD_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, name+".png"), b, 0o644)
		}
	}
}

func TestPickGPU(t *testing.T) {
	u := 5.0
	g := []cardGPU{{Name: "Intel", VRAMTotal: 50e9}, {Name: "NVIDIA", VRAMTotal: 16e9, Usage: &u}}
	if i := pickGPU(g); i != 1 {
		t.Fatalf("want the GPU that reports load, got %d", i)
	}
	if i := pickGPU(nil); i != -1 {
		t.Fatalf("want -1 without GPUs, got %d", i)
	}
}

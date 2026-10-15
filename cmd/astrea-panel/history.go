package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// History of CPU/RAM/GPU usage per machine, sampled by the panel itself so the
// Telegram cards and the web charts have a past even right after opening.
const (
	historyStep = 15 * time.Second
	historyLen  = 480 // 2 h
)

// cardHistory is one machine's recent usage, 0–100, oldest first, one value
// per Step. A negative value means no sample (machine off or not answering).
type cardHistory struct {
	CPU, RAM, GPU []float64
	Step          time.Duration
	End           time.Time // time of the newest sample
}

// ring is a fixed-size series.
type ring struct {
	cpu, ram, gpu [historyLen]float64
	n, next       int
	end           time.Time
}

func (r *ring) add(cpu, ram, gpu float64, at time.Time) {
	r.cpu[r.next], r.ram[r.next], r.gpu[r.next] = cpu, ram, gpu
	r.next = (r.next + 1) % historyLen
	if r.n < historyLen {
		r.n++
	}
	r.end = at
}

func (r *ring) snapshot() cardHistory {
	h := cardHistory{Step: historyStep, End: r.end,
		CPU: make([]float64, r.n), RAM: make([]float64, r.n), GPU: make([]float64, r.n)}
	start := (r.next - r.n + historyLen) % historyLen
	for i := 0; i < r.n; i++ {
		j := (start + i) % historyLen
		h.CPU[i], h.RAM[i], h.GPU[i] = r.cpu[j], r.ram[j], r.gpu[j]
	}
	return h
}

type historyStore struct {
	mu    sync.Mutex
	rings map[string]*ring
}

// history returns the recent usage of a machine (empty if never sampled).
func (p *panel) history(name string) cardHistory {
	p.hist.mu.Lock()
	defer p.hist.mu.Unlock()
	if r := p.hist.rings[name]; r != nil {
		return r.snapshot()
	}
	return cardHistory{Step: historyStep}
}

// sampleLoop records every machine's usage each historyStep until ctx ends.
func (p *panel) sampleLoop(ctx context.Context) {
	t := time.NewTicker(historyStep)
	defer t.Stop()
	for {
		p.sampleOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *panel) sampleOnce(ctx context.Context) {
	machines := p.reload()
	now := time.Now()
	var wg sync.WaitGroup
	type sample struct{ cpu, ram, gpu float64 }
	got := make([]sample, len(machines))
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = sample{-1, -1, -1}
			client := p.client(m.Name)
			if client == nil {
				return
			}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			res, err := client.Call(cctx, "sys.stats", map[string]any{})
			if err != nil {
				return
			}
			p.mu.Lock()
			p.lastSeen[m.Name] = time.Now().UnixMilli()
			p.mu.Unlock()
			var st struct {
				CPU struct {
					Usage float64 `json:"usage_percent"`
				} `json:"cpu"`
				Memory struct {
					UsedPercent float64 `json:"used_percent"`
				} `json:"memory"`
				GPUs []struct {
					Usage     *float64 `json:"usage_percent"`
					VRAMTotal uint64   `json:"vram_total"`
					VRAMUsed  *uint64  `json:"vram_used"`
				} `json:"gpus"`
			}
			if json.Unmarshal(res.Result, &st) != nil {
				return
			}
			s := sample{st.CPU.Usage, st.Memory.UsedPercent, -1}
			if len(st.GPUs) > 0 {
				g := st.GPUs[0]
				switch {
				case g.Usage != nil:
					s.gpu = *g.Usage
				case g.VRAMUsed != nil && g.VRAMTotal > 0:
					s.gpu = 100 * float64(*g.VRAMUsed) / float64(g.VRAMTotal)
				}
			}
			got[i] = s
		}()
	}
	wg.Wait()

	p.hist.mu.Lock()
	defer p.hist.mu.Unlock()
	keep := map[string]bool{}
	for i, m := range machines {
		keep[m.Name] = true
		r := p.hist.rings[m.Name]
		if r == nil {
			r = &ring{}
			p.hist.rings[m.Name] = r
		}
		r.add(got[i].cpu, got[i].ram, got[i].gpu, now)
	}
	for name := range p.hist.rings { // machines removed from the config
		if !keep[name] {
			delete(p.hist.rings, name)
		}
	}
}

// handleHistory: GET /api/history?machine= → the series for charts.
func (p *panel) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("machine")
	if p.client(name) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": errUnknownMachine.Error()})
		return
	}
	h := p.history(name)
	out := map[string]any{"step_sec": int(h.Step / time.Second), "cpu": h.CPU, "ram": h.RAM, "gpu": h.GPU}
	if !h.End.IsZero() {
		out["ts_end"] = h.End.UnixMilli()
	}
	writeJSON(w, http.StatusOK, out)
}

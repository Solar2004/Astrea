package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
)

// runGateway serves an OpenAI-compatible HTTP API in front of every model
// server exposed across the fleet. Each service with an exposed endpoint
// becomes a model named "<machine>/<endpoint>"; requests are reverse-proxied
// to that agent's /dyn/<endpoint>, which already fronts the local server.
//
// This turns the Windows PCs' local models into one endpoint the AI (or any
// OpenAI client) can call, choosing the model — hence the machine — by name.
func runGateway(argv []string) {
	fs := flag.NewFlagSet("astrea-hub gateway", flag.ExitOnError)
	cfgPath := fs.String("config", fleet.DefaultPath(), "machines file")
	listen := fs.String("listen", "127.0.0.1:7788", "address to serve the OpenAI-compatible API on")
	refresh := fs.Duration("refresh", 15*time.Second, "how often to rediscover exposed models")
	_ = fs.Parse(argv)

	logger := log.New(os.Stderr, "astrea-gateway ", log.LstdFlags)
	machines, err := fleet.Load(*cfgPath)
	if err != nil {
		logger.Fatalf("load machines: %v", err)
	}
	g := &gateway{fleet: fleet.New(machines), logger: logger, models: map[string]model{}}
	g.discover(context.Background())
	go func() {
		for range time.Tick(*refresh) {
			g.discover(context.Background())
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", g.handleModels)
	mux.HandleFunc("/v1/", g.handleProxy) // chat/completions, completions, embeddings
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listen, err)
	}
	logger.Printf("astrea-gateway %s on http://%s/v1 — %d model(s) across %v", version, ln.Addr(), g.countModels(), g.fleet.Names())
	logger.Fatal(http.Serve(ln, mux))
}

// model is one exposed model server.
type model struct {
	id       string // "<machine>/<endpoint>"
	machine  string
	endpoint string
}

type gateway struct {
	fleet  *fleet.Fleet
	logger *log.Logger

	mu     sync.RWMutex
	models map[string]model
}

func (g *gateway) countModels() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.models)
}

// discover asks every machine for its running services with an exposed
// endpoint and rebuilds the model map.
func (g *gateway) discover(ctx context.Context) {
	found := map[string]model{}
	var mu sync.Mutex
	g.fleet.Each(func(n *fleet.Node) {
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		var out struct {
			Services []struct {
				Name   string `json:"name"`
				State  string `json:"state"`
				Expose *struct {
					Endpoint string `json:"endpoint"`
				} `json:"expose"`
			} `json:"services"`
		}
		if err := n.Call(cctx, "service.status", &out); err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, s := range out.Services {
			if s.State != "running" || s.Expose == nil || s.Expose.Endpoint == "" {
				continue
			}
			id := n.Name + "/" + s.Expose.Endpoint
			found[id] = model{id: id, machine: n.Name, endpoint: s.Expose.Endpoint}
		}
	})
	g.mu.Lock()
	g.models = found
	g.mu.Unlock()
}

func (g *gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	ids := make([]string, 0, len(g.models))
	for id := range g.models {
		ids = append(ids, id)
	}
	g.mu.RUnlock()
	sort.Strings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "astrea"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleProxy rewrites an OpenAI request's "model" to strip the "<machine>/"
// prefix and reverse-proxies it to that machine's /dyn/<endpoint>/v1/...
func (g *gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var requested string
	_ = json.Unmarshal(payload["model"], &requested)

	m, localModel, err := g.resolve(requested)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// Rewrite "model" to the name the local server knows (after the slash),
	// unless the caller addressed a bare machine ("<machine>/") — then leave
	// whatever the local server defaults to.
	if localModel != "" {
		payload["model"], _ = json.Marshal(localModel)
		body, _ = json.Marshal(payload)
	}

	n := g.fleet.Get(m.machine)
	if n == nil {
		writeErr(w, http.StatusBadGateway, "machine offline: "+m.machine)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/")
	target := n.Base + "/dyn/" + m.endpoint + "/v1/" + suffix

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream "+m.machine+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // streams SSE as it arrives
}

// resolve maps a requested model id to a target model and the model name the
// local server should see. Forms:
//
//	"machine/endpoint"       -> that model, pass its default model through
//	"machine/endpoint:llama" -> that model, ask the server for "llama"
//	"endpoint" / "llama"     -> unique match across the fleet, else error
func (g *gateway) resolve(requested string) (model, string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.models) == 0 {
		return model{}, "", fmt.Errorf("no models available; start a service with an exposed endpoint")
	}
	id, local := requested, ""
	if i := strings.Index(requested, ":"); i >= 0 {
		id, local = requested[:i], requested[i+1:]
	}
	if m, ok := g.models[id]; ok {
		return m, local, nil
	}
	// Match by endpoint or "<machine>" alone across the fleet.
	var hits []model
	for _, m := range g.models {
		if m.endpoint == id || m.machine == id || id == "" {
			hits = append(hits, m)
		}
	}
	if len(hits) == 1 {
		return hits[0], local, nil
	}
	if len(hits) > 1 {
		ids := make([]string, len(hits))
		for i, m := range hits {
			ids[i] = m.id
		}
		sort.Strings(ids)
		return model{}, "", fmt.Errorf("ambiguous model %q; use one of %v", requested, ids)
	}
	return model{}, "", fmt.Errorf("unknown model %q", requested)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "astrea_gateway"}})
}

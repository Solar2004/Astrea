package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/Solar2004/Astrea/internal/update"
)

// latestCache remembers the newest release tag for a few hours.
type latestCache struct {
	mu      sync.Mutex
	tag     string
	checked time.Time
}

func (c *latestCache) get(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tag != "" && time.Since(c.checked) < 3*time.Hour {
		return c.tag
	}
	if time.Since(c.checked) < time.Minute {
		return c.tag // failed recently; do not hammer GitHub on every refresh
	}
	c.checked = time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if tag, err := update.Latest(ctx, update.Base()); err == nil {
		c.tag = tag
	}
	return c.tag
}

// handleVersion: GET /api/version → panel version and the latest release.
func (p *panel) handleVersion(w http.ResponseWriter, r *http.Request) {
	latest := p.latest.get(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"panel":            version,
		"latest":           latest,
		"update_available": update.Newer(version, latest),
	})
}

// handleUpdate: POST /api/update?machine=NAME updates that machine's agent;
// machine=all updates the whole fleet; machine=hub updates hub and panel on
// this PC (the panel restarts, so the response may not arrive).
func (p *panel) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
		return
	}
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request refused"})
		return
	}
	force := r.URL.Query().Get("force") == "1"
	switch name := r.URL.Query().Get("machine"); name {
	case "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "falta ?machine="})
	case "hub":
		hub := filepath.Join(filepath.Dir(selfPath()), "astrea-hub")
		args := []string{"update", "--local", "--json"}
		if force {
			args = append(args, "--force")
		}
		out, err := exec.CommandContext(r.Context(), hub, args...).Output()
		if err != nil && len(out) == 0 {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	case "all":
		machines := p.reload()
		res := make([]map[string]any, len(machines))
		var wg sync.WaitGroup
		for i, m := range machines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = p.updateAgent(r.Context(), m.Name, force)
			}()
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{"results": res})
	default:
		if p.client(name) == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": errUnknownMachine.Error()})
			return
		}
		res := p.updateAgent(r.Context(), name, force)
		code := http.StatusOK
		if _, bad := res["error"]; bad {
			code = http.StatusBadGateway
		}
		writeJSON(w, code, res)
	}
}

// updateAgent calls agent.update on one machine.
func (p *panel) updateAgent(ctx context.Context, name string, force bool) map[string]any {
	out := map[string]any{"machine": name}
	client := p.client(name)
	if client == nil {
		out["error"] = errUnknownMachine.Error()
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := client.Call(ctx, "agent.update", map[string]any{"force": force})
	if err != nil && strings.Contains(err.Error(), "unknown method") {
		// v0.2.x agent: migrate it once through exec; it self-updates after.
		latest := p.latest.get(ctx)
		if latest == "" {
			out["error"] = "no pude consultar la última versión en GitHub"
			return out
		}
		m := p.machine(name)
		if m == nil {
			out["error"] = errUnknownMachine.Error()
			return out
		}
		node := fleet.New([]fleet.Machine{fleet.Machine(*m)}).Get(name)
		r := node.Migrate(ctx, latest)
		out["migrated"] = true
		out["target"], out["updated"], out["note"] = r.Target, r.Updated, r.Note
		if r.Error != "" {
			out["error"] = r.Error
		} else {
			out["ok"] = true
		}
		return out
	}
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	var r map[string]any
	_ = json.Unmarshal(res.Result, &r)
	for k, v := range r {
		out[k] = v
	}
	out["ok"] = true
	return out
}

// sameOrigin blocks other websites from driving the panel through the
// user's browser: a POST must come from the panel's own page.
func sameOrigin(r *http.Request) bool {
	// DNS rebinding: a hostile domain resolving to 127.0.0.1 would look
	// same-origin. The panel is only ever opened by IP or as localhost.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "localhost" && net.ParseIP(host) == nil {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients (curl) send no Origin; browsers always do on POST.
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func selfPath() string {
	p, _ := update.Self()
	return p
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

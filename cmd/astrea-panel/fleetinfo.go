package main

// Fleet history and layout:
//
//	GET  /api/uptime?machine=&days=   sys.uptime of one machine (or all, without machine)
//	GET  /api/work/shot?machine=&id=  screenshot taken when a task failed
//	POST /api/cleanup?machine=&dry_run=  disk.cleanup
//	GET/PUT /api/ui                   groups and view settings, shared by every browser
//	GET  /api/thumb?machine=&w=&max_age=  small cached screenshot for cards and the wall
//
// The uptime lives on each agent; the panel keeps the last copy it got, so a
// machine that is off still shows its history. Connection quality comes from
// `tailscale ping` (direct or through a DERP relay, and latency).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- uptime ---

type uptimeCache struct {
	mu   sync.Mutex
	path string
	M    map[string]cachedUptime `json:"machines"`
}

type cachedUptime struct {
	Report json.RawMessage `json:"report"`
	Days   int             `json:"days"`
	At     time.Time       `json:"at"`
}

func loadUptimeCache() *uptimeCache {
	c := &uptimeCache{M: map[string]cachedUptime{}}
	if dir, err := os.UserCacheDir(); err == nil {
		c.path = filepath.Join(dir, "astrea", "panel-uptime.json")
		if data, err := os.ReadFile(c.path); err == nil {
			_ = json.Unmarshal(data, c)
		}
	}
	if c.M == nil {
		c.M = map[string]cachedUptime{}
	}
	return c
}

func (c *uptimeCache) get(name string) (cachedUptime, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.M[name]
	return v, ok
}

func (c *uptimeCache) put(name string, v cachedUptime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.M[name] = v
	if c.path == "" {
		return
	}
	data, _ := json.Marshal(c)
	_ = os.MkdirAll(filepath.Dir(c.path), 0o700)
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

// uptime returns a machine's report: fresh (≤1 min old) from the cache, else
// asked to the agent, else the last copy marked stale.
func (p *panel) uptime(ctx context.Context, name string, days int) map[string]any {
	c, ok := p.uptimes.get(name)
	if ok && c.Days >= days && time.Since(c.At) < time.Minute {
		return map[string]any{"machine": name, "report": c.Report, "cached_at": c.At}
	}
	// The OS log can take a few seconds on the first call; a machine that has
	// not answered lately fails fast and the cached copy is served.
	timeout := 25 * time.Second
	if p.workTimeout(name) < 8*time.Second {
		timeout = 3 * time.Second
	}
	var raw json.RawMessage
	_, err := p.call(ctx, name, "sys.uptime", map[string]any{"days": days}, &raw, timeout)
	if err == nil {
		p.uptimes.put(name, cachedUptime{Report: raw, Days: days, At: time.Now()})
		return map[string]any{"machine": name, "report": raw, "cached_at": time.Now()}
	}
	out := map[string]any{"machine": name, "error": err.Error()}
	if ok {
		out["report"], out["cached_at"], out["stale"] = c.Report, c.At, true
	}
	return out
}

func (p *panel) handleUptime(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := queryInt(r, "days", 30, 1, 120)
	if name := q.Get("machine"); name != "" {
		if p.client(name) == nil {
			fail(w, http.StatusNotFound, errUnknownMachine)
			return
		}
		writeJSON(w, http.StatusOK, p.uptime(r.Context(), name, days))
		return
	}
	machines := p.reload()
	out := make([]map[string]any, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = p.uptime(r.Context(), m.Name, days)
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

// --- connection quality ---

// netQuality is one `tailscale ping` result.
type netQuality struct {
	MS     int       `json:"ms,omitempty"`
	Direct bool      `json:"direct"`
	Via    string    `json:"via,omitempty"` // endpoint ip:port, or the DERP region
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

var rePong = regexp.MustCompile(`pong from \S+ \(\S+\) via (\S+(?:\([^)]*\))?) in (\d+(?:\.\d+)?)ms`)

func parsePing(out string) netQuality {
	q := netQuality{At: time.Now()}
	m := rePong.FindStringSubmatch(out)
	if m == nil {
		q.Error = "sin respuesta"
		return q
	}
	ms, _ := strconv.ParseFloat(m[2], 64)
	q.MS = max(int(ms+0.5), 1)
	q.Via = m[1]
	if r := regexp.MustCompile(`^DERP\(([^)]*)\)$`).FindStringSubmatch(m[1]); r != nil {
		q.Via = r[1]
	} else {
		q.Direct = true
	}
	return q
}

// netLoop pings every machine over the tailnet every 2 minutes.
func (p *panel) netLoop(ctx context.Context) {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		return
	}
	for {
		var wg sync.WaitGroup
		for _, m := range p.reload() {
			u, err := url.Parse(m.WS)
			if err != nil || !onTailnet(u.Hostname()) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
				defer cancel()
				// One lost ping is noise (a path being renegotiated): retry once.
				var q netQuality
				for try := 0; try < 2; try++ {
					out, _ := exec.CommandContext(cctx, bin, "ping", "--c", "1", "--timeout", "4s", u.Hostname()).CombinedOutput()
					if q = parsePing(string(out)); q.Error == "" {
						break
					}
				}
				p.nets.Store(m.Name, q)
			}()
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
	}
}

// cgnat is Tailscale's address range (100.64.0.0/10).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// onTailnet reports whether host is a tailnet address (IP or MagicDNS name).
func onTailnet(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return cgnat.Contains(ip)
	}
	return strings.HasSuffix(host, ".ts.net")
}

func (p *panel) net(name string) *netQuality {
	if v, ok := p.nets.Load(name); ok {
		q := v.(netQuality)
		return &q
	}
	return nil
}

// --- thumbnails ---

// thumb is a small, recent screenshot shared by every browser looking at the
// overview or the wall of screens, so N tabs do not mean N captures.
type thumb struct {
	data []byte
	at   time.Time
	err  error
}

var (
	thumbsMu sync.Mutex
	thumbs   = map[string]*thumb{}
	thumbsIn = map[string]chan struct{}{} // capture in flight
)

// handleThumb: GET /api/thumb?machine=&w=480&max_age=20 → JPEG.
func (p *panel) handleThumb(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("machine")
	if p.client(name) == nil {
		http.Error(w, errUnknownMachine.Error(), http.StatusNotFound)
		return
	}
	width := queryInt(r, "w", 480, 160, 1280)
	maxAge := time.Duration(queryInt(r, "max_age", 20, 2, 600)) * time.Second
	key := name + "@" + strconv.Itoa(width)
	for {
		thumbsMu.Lock()
		t := thumbs[key]
		if t != nil && time.Since(t.at) < maxAge {
			thumbsMu.Unlock()
			if t.err != nil {
				http.Error(w, t.err.Error(), http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Captured-At", t.at.UTC().Format(time.RFC3339))
			_, _ = w.Write(t.data)
			return
		}
		if wait, busy := thumbsIn[key]; busy {
			thumbsMu.Unlock()
			select {
			case <-wait:
				continue
			case <-r.Context().Done():
				return
			}
		}
		done := make(chan struct{})
		thumbsIn[key] = done
		thumbsMu.Unlock()

		timeout := 20 * time.Second
		if p.workTimeout(name) < 8*time.Second {
			timeout = 3 * time.Second
		}
		data, err := p.frame(context.Background(), name, width, 55, timeout, true)
		thumbsMu.Lock()
		thumbs[key] = &thumb{data: data, at: time.Now(), err: err}
		delete(thumbsIn, key)
		thumbsMu.Unlock()
		close(done)
	}
}

// --- failure screenshots, cleanup ---

func (p *panel) handleWorkShot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var res struct {
		Images []Image `json:"images"`
	}
	if code, err := p.call(r.Context(), q.Get("machine"), "work.shot", map[string]any{"id": q.Get("id")}, &res, 20*time.Second); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
	if len(res.Images) == 0 {
		http.Error(w, "sin imagen", http.StatusNotFound)
		return
	}
	data, err := base64.StdEncoding.DecodeString(res.Images[0].Data)
	if err != nil {
		http.Error(w, "imagen inválida", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", res.Images[0].MimeType)
	w.Header().Set("Cache-Control", "private, max-age=86400") // a task's shot never changes
	_, _ = w.Write(data)
}

// Image is an image in a capability result.
type Image struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

func (p *panel) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if !mutating(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	dry, _ := strconv.ParseBool(q.Get("dry_run"))
	var res map[string]any
	if code, err := p.call(r.Context(), q.Get("machine"), "disk.cleanup", map[string]any{"dry_run": dry}, &res, 5*time.Minute); err != nil {
		fail(w, code, err)
		return
	}
	if !dry {
		p.logger.Printf("limpieza de disco en %s (desde el panel)", q.Get("machine"))
	}
	writeJSON(w, http.StatusOK, res)
}

// --- groups and view settings ---

// handleUI stores the panel layout (groups of machines, grid/list, collapsed
// sections…) next to the machines file, so every browser and phone sees the
// same. The panel does not interpret it beyond being a JSON object.
func (p *panel) handleUI(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(filepath.Dir(p.cfgPath), "panel-ui.json")
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			data = []byte("{}")
		} else if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	case http.MethodPut:
		if !mutating(w, r, http.MethodPut) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		var obj map[string]json.RawMessage
		if err != nil || json.Unmarshal(body, &obj) != nil {
			fail(w, http.StatusBadRequest, errors.New("se esperaba un objeto JSON"))
			return
		}
		p.editMu.Lock()
		defer p.editMu.Unlock()
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("use GET o PUT"))
	}
}

// Command astrea-panel is a local web dashboard for the operator's machine: it
// shows the hardware (CPU, RAM, GPU, disks) of one or more astrea-agents and
// their live usage. It talks to each agent over the tailnet with the same
// token the hub uses and serves the page on localhost only, so the token never
// reaches the browser.
//
//	astrea-panel --ws ws://100.x.y.z:7700/ws --token <TOKEN>
//	astrea-panel   # reads ~/.config/astrea/panel.json (several machines)
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/transport"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed web
var webFS embed.FS

// Machine is one agent shown in the panel.
type Machine struct {
	Name  string `json:"name"`
	WS    string `json:"ws"`
	Token string `json:"token"`
}

type panelConfig struct {
	Machines []Machine `json:"machines"`
}

type machineStatus struct {
	Name     string          `json:"name"`
	Base     string          `json:"base,omitempty"` // http://host:port del agente (para enlazar /dyn)
	Online   bool            `json:"online"`
	Error    string          `json:"error,omitempty"`
	Stats    json.RawMessage `json:"stats,omitempty"`
	Idle     json.RawMessage `json:"idle,omitempty"`     // sys.idle: quién usa la PC
	Services json.RawMessage `json:"services,omitempty"` // service.status: servicios/modelos
	Admin    json.RawMessage `json:"admin,omitempty"`    // admin.status: os, modo, kill switch
}

type panel struct {
	ws, token, name, cfgPath string
	logger                   *log.Logger

	mu       sync.Mutex
	machines []Machine
	clients  map[string]*transport.ReconnectingClient // keyed by ws URL, reused across reloads
}

func main() {
	ws := flag.String("ws", os.Getenv("ASTREA_WS"), "agent WebSocket URL, e.g. ws://100.x.y.z:7700/ws (single machine)")
	token := flag.String("token", os.Getenv("ASTREA_TOKEN"), "agent token (or ASTREA_TOKEN env)")
	name := flag.String("name", "", "display name for the --ws machine (default: its hostname)")
	cfgPath := flag.String("config", defaultConfigPath(), "JSON file listing machines: {\"machines\":[{\"name\",\"ws\",\"token\"}]}")
	listen := flag.String("listen", "127.0.0.1:7780", "address to serve the panel on")
	open := flag.Bool("open", true, "open the panel in the browser")
	flag.Parse()

	logger := log.New(os.Stderr, "astrea-panel ", log.LstdFlags)

	p := &panel{ws: *ws, token: *token, name: *name, cfgPath: *cfgPath, logger: logger, clients: map[string]*transport.ReconnectingClient{}}
	// Load once now (for the startup log), but the stats handler reloads on
	// every request, so adding machines to panel.json needs no restart and an
	// empty/missing config is not fatal — the panel just shows nothing yet.
	if n := len(p.reload()); n == 0 {
		logger.Printf("no machines yet (add them to %s; the panel picks them up live)", *cfgPath)
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/stats", p.handleStats)
	mux.HandleFunc("/api/screenshot", p.handleScreenshot)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listen, err)
	}
	url := "http://" + ln.Addr().String()
	logger.Printf("astrea-panel %s: %d machine(s) — open %s", version, len(p.machines), url)
	if *open {
		go openBrowser(url)
	}
	logger.Fatal(http.Serve(ln, mux))
}

// reload re-reads the machine list (--ws, else the config file, else none),
// reusing existing clients and dialing new ones. It never fails: a missing or
// bad config means zero machines until it is fixed. Returns the current list.
func (p *panel) reload() []Machine {
	machines := loadMachines(p.ws, p.token, p.name, p.cfgPath, p.logger)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range machines {
		if _, ok := p.clients[m.WS]; !ok {
			p.clients[m.WS] = transport.NewReconnecting(m.WS, m.Token)
		}
	}
	p.machines = machines
	return machines
}

// loadMachines resolves the machine list without failing: on any problem it
// logs once and returns nil.
func loadMachines(ws, token, name, cfgPath string, logger *log.Logger) []Machine {
	if ws != "" {
		return []Machine{{Name: name, WS: ws, Token: token}}
	}
	data, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		logger.Printf("read %s: %v", cfgPath, err)
		return nil
	}
	var c panelConfig
	if err := json.Unmarshal(data, &c); err != nil {
		logger.Printf("%s: %v", cfgPath, err)
		return nil
	}
	return c.Machines
}

// handleStats polls every machine in parallel and returns their snapshots.
func (p *panel) handleStats(w http.ResponseWriter, r *http.Request) {
	machines := p.reload() // pick up panel.json edits without a restart
	out := make([]machineStatus, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func(i int, m Machine) {
			defer wg.Done()
			out[i] = p.poll(r.Context(), m)
		}(i, m)
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (p *panel) poll(ctx context.Context, m Machine) machineStatus {
	st := machineStatus{Name: m.Name, Base: baseURL(m.WS)}
	p.mu.Lock()
	client := p.clients[m.WS]
	p.mu.Unlock()
	if client == nil {
		st.Error = "no client"
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	// admin.status is cheap and always present: it confirms the agent is alive.
	admin, err := client.Call(ctx, "admin.status", map[string]any{})
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Online = true
	st.Admin = admin.Result
	// The rest in parallel; a missing/failed capability is not fatal.
	var wg sync.WaitGroup
	get := func(method string, dst *json.RawMessage) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, e := client.Call(ctx, method, map[string]any{}); e == nil {
				*dst = r.Result
			}
		}()
	}
	get("sys.stats", &st.Stats)
	get("sys.idle", &st.Idle)
	get("service.status", &st.Services)
	wg.Wait()
	return st
}

// handleScreenshot proxies a live screen capture of one machine as a JPEG.
func (p *panel) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("machine")
	p.mu.Lock()
	var client *transport.ReconnectingClient
	for _, m := range p.machines {
		if m.Name == name {
			client = p.clients[m.WS]
		}
	}
	p.mu.Unlock()
	if client == nil {
		http.Error(w, "máquina desconocida", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res, err := client.Call(ctx, "screen.capture", map[string]any{"max_width": 1280, "format": "jpeg", "quality": 70})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var out struct {
		Images []struct {
			MimeType string `json:"mime_type"`
			Data     string `json:"data"`
		} `json:"images"`
	}
	if json.Unmarshal(res.Result, &out) != nil || len(out.Images) == 0 {
		http.Error(w, "esta PC no devolvió imagen (¿sin escritorio/sesión?)", http.StatusBadGateway)
		return
	}
	data, err := base64.StdEncoding.DecodeString(out.Images[0].Data)
	if err != nil {
		http.Error(w, "imagen inválida", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", out.Images[0].MimeType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func defaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "astrea", "panel.json")
	}
	return "panel.json"
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// baseURL maps ws://host:port/ws -> http://host:port so the browser can link
// to the agent's /dyn endpoints (the token never goes to the browser).
func baseURL(ws string) string {
	u, err := url.Parse(ws)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme == "wss" {
		return "https://" + u.Host
	}
	return "http://" + u.Host
}

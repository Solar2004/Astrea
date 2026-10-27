// Command astrea-panel is a local web dashboard for the operator's machine: it
// shows the hardware (CPU, RAM, GPU, disks) of one or more astrea-agents and
// their live usage. It talks to each agent over the tailnet with the same
// token the hub uses and serves the page on localhost only, so the token never
// reaches the browser.
//
//	astrea-panel --ws ws://100.x.y.z:7700/ws --token <TOKEN>
//	astrea-panel   # reads ~/.config/astrea/panel.json (several machines)
//
// It watches the fleet and records notices (events.go); with telegram.json
// next to the config it sends them to Telegram (telegram.go), with hermes.json
// it wakes Hermes through its webhook (hermes.go).
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
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
	Idle     json.RawMessage `json:"idle,omitempty"`      // sys.idle: quién usa la PC
	Services json.RawMessage `json:"services,omitempty"`  // service.status: servicios/modelos
	Admin    json.RawMessage `json:"admin,omitempty"`     // admin.status: os, modo, kill switch
	LastSeen int64           `json:"last_seen,omitempty"` // unix ms de la última respuesta
	Net      *netQuality     `json:"net,omitempty"`       // tailscale ping: directa o por relé, latencia
}

type panel struct {
	ws, token, name, cfgPath string
	logger                   *log.Logger

	mu       sync.Mutex
	machines []Machine
	clients  map[string]*transport.ReconnectingClient // keyed by ws URL, reused across reloads
	latest   latestCache
	lastSeen map[string]int64 // machine name → unix ms of the last answer
	editMu   sync.Mutex       // serializes edits of the machines file
	hist     historyStore     // CPU/RAM/GPU over the last 2 h (history.go)
	work     *workCache       // last known tasks per machine (workcache.go)
	events   *eventLog        // fleet notices (events.go)
	uptimes  *uptimeCache     // last sys.uptime per machine (fleetinfo.go)
	nets     sync.Map         // machine name → netQuality
}

func main() {
	ws := flag.String("ws", os.Getenv("ASTREA_WS"), "agent WebSocket URL, e.g. ws://100.x.y.z:7700/ws (single machine)")
	token := flag.String("token", os.Getenv("ASTREA_TOKEN"), "agent token (or ASTREA_TOKEN env)")
	name := flag.String("name", "", "display name for the --ws machine (default: its hostname)")
	cfgPath := flag.String("config", defaultConfigPath(), "JSON file listing machines: {\"machines\":[{\"name\",\"ws\",\"token\"}]}")
	listen := flag.String("listen", "127.0.0.1:7780", "address to serve the panel on")
	open := flag.Bool("open", true, "open the panel in the browser")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("astrea-panel %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	logger := log.New(os.Stderr, "astrea-panel ", log.LstdFlags)

	p := &panel{ws: *ws, token: *token, name: *name, cfgPath: *cfgPath, logger: logger, clients: map[string]*transport.ReconnectingClient{}, lastSeen: map[string]int64{}, hist: historyStore{rings: map[string]*ring{}}, work: loadWorkCache(workCachePath()), events: newEventLog(), uptimes: loadUptimeCache()}
	// Load once now (for the startup log), but the stats handler reloads on
	// every request, so adding machines to panel.json needs no restart and an
	// empty/missing config is not fatal — the panel just shows nothing yet.
	if n := len(p.reload()); n == 0 {
		logger.Printf("no machines yet (add them to %s; the panel picks them up live)", *cfgPath)
	}
	go p.sampleLoop(context.Background())
	startTelegram(p, telegramConfigPath(*cfgPath), logger)
	startHermes(p, hermesConfigPath(*cfgPath), logger)
	go p.watch(20 * time.Second)
	go p.netLoop(context.Background())

	// Go does not know .webmanifest; browsers want this type for the PWA.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	files := http.FileServer(http.FS(static))
	// no-cache: tras actualizar el panel, el navegador revalida y no sirve una web vieja.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/api/stats", p.handleStats)
	mux.HandleFunc("/api/screenshot", p.handleScreenshot)
	mux.HandleFunc("/api/version", p.handleVersion)
	mux.HandleFunc("/api/update", p.handleUpdate)
	mux.HandleFunc("/api/lock", p.handleLock)
	mux.HandleFunc("/api/power", p.handlePower)
	mux.HandleFunc("/api/history", p.handleHistory)
	mux.HandleFunc("/api/machines", p.handleMachines)
	mux.HandleFunc("/api/audit", p.handleAudit)
	mux.HandleFunc("/api/work", p.handleWork)
	mux.HandleFunc("/api/work/cancel", p.handleWorkCancel)
	mux.HandleFunc("/api/service", p.handleService)
	mux.HandleFunc("/api/service/logs", p.handleServiceLogs)
	mux.HandleFunc("/api/events", p.handleEvents)
	mux.HandleFunc("/api/live", p.handleLive)
	mux.HandleFunc("/api/control", p.handleControl)
	mux.HandleFunc("/api/input", p.handleInput)
	mux.HandleFunc("/api/memory", p.handleMemory)
	mux.HandleFunc("/api/uptime", p.handleUptime)
	mux.HandleFunc("/api/work/shot", p.handleWorkShot)
	mux.HandleFunc("/api/cleanup", p.handleCleanup)
	mux.HandleFunc("/api/ui", p.handleUI)
	mux.HandleFunc("/api/thumb", p.handleThumb)

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
	st := machineStatus{Name: m.Name, Base: baseURL(m.WS), Net: p.net(m.Name)}
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
	p.mu.Lock()
	if err == nil {
		p.lastSeen[m.Name] = time.Now().UnixMilli()
	}
	st.LastSeen = p.lastSeen[m.Name]
	p.mu.Unlock()
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
	data, mime, err := p.capture(r.Context(), r.URL.Query().Get("machine"))
	if errors.Is(err, errUnknownMachine) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

var errUnknownMachine = errors.New("máquina desconocida")

// machine returns the configured machine with that name, or nil.
func (p *panel) machine(name string) *Machine {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.machines {
		if m.Name == name {
			return &m
		}
	}
	return nil
}

// client returns the agent client of the machine with that name, or nil.
func (p *panel) client(name string) *transport.ReconnectingClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.machines {
		if m.Name == name {
			return p.clients[m.WS]
		}
	}
	return nil
}

// capture takes a live screenshot of one machine; returns the image bytes and
// their MIME type. Shared by the web panel and the Telegram bot.
func (p *panel) capture(ctx context.Context, name string) ([]byte, string, error) {
	client := p.client(name)
	if client == nil {
		return nil, "", errUnknownMachine
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	res, err := client.Call(ctx, "screen.capture", map[string]any{"max_width": 1920, "format": "jpeg", "quality": 75})
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Images []struct {
			MimeType string `json:"mime_type"`
			Data     string `json:"data"`
		} `json:"images"`
	}
	if json.Unmarshal(res.Result, &out) != nil || len(out.Images) == 0 {
		return nil, "", errors.New("esta PC no devolvió imagen (¿sin escritorio/sesión?)")
	}
	data, err := base64.StdEncoding.DecodeString(out.Images[0].Data)
	if err != nil {
		return nil, "", errors.New("imagen inválida")
	}
	return data, out.Images[0].MimeType, nil
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

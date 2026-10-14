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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
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
	Name   string          `json:"name"`
	Online bool            `json:"online"`
	Error  string          `json:"error,omitempty"`
	Stats  json.RawMessage `json:"stats,omitempty"`
}

type panel struct {
	machines []Machine
	clients  []*transport.ReconnectingClient
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

	machines, err := loadMachines(*ws, *token, *name, *cfgPath)
	if err != nil {
		logger.Fatal(err)
	}
	p := &panel{machines: machines}
	for _, m := range machines {
		p.clients = append(p.clients, transport.NewReconnecting(m.WS, m.Token))
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/stats", p.handleStats)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Fatalf("listen %s: %v", *listen, err)
	}
	url := "http://" + ln.Addr().String()
	logger.Printf("astrea-panel %s: %d machine(s) — open %s", version, len(machines), url)
	if *open {
		go openBrowser(url)
	}
	logger.Fatal(http.Serve(ln, mux))
}

func loadMachines(ws, token, name, cfgPath string) ([]Machine, error) {
	if ws != "" {
		return []Machine{{Name: name, WS: ws, Token: token}}, nil
	}
	data, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no machines: pass --ws and --token, or create %s with {\"machines\":[{\"name\":\"windows\",\"ws\":\"ws://100.x.y.z:7700/ws\",\"token\":\"...\"}]}", cfgPath)
	}
	if err != nil {
		return nil, err
	}
	var c panelConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", cfgPath, err)
	}
	if len(c.Machines) == 0 {
		return nil, fmt.Errorf("%s lists no machines", cfgPath)
	}
	return c.Machines, nil
}

// handleStats polls every machine in parallel and returns their snapshots.
func (p *panel) handleStats(w http.ResponseWriter, r *http.Request) {
	out := make([]machineStatus, len(p.machines))
	var wg sync.WaitGroup
	for i := range p.machines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = p.poll(r.Context(), i)
		}(i)
	}
	wg.Wait()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (p *panel) poll(ctx context.Context, i int) machineStatus {
	st := machineStatus{Name: p.machines[i].Name}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	res, err := p.clients[i].Call(ctx, "sys.stats", map[string]any{})
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Online = true
	st.Stats = res.Result
	return st
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

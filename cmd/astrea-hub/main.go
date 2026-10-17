// Command astrea-hub connects the AI to every astrea-agent in the fleet.
//
// Default mode is an MCP server (stdio) that Hermes loads: it mirrors the
// agents' capabilities as MCP tools — one tool per capability, with a
// "machine" argument when several machines have it — and forwards each call
// to the chosen agent. Capabilities are cached on disk per machine so tools
// are listed even while an agent is down, and the tool list follows the
// agents live (tasks the AI defines show up as new tools).
//
//	astrea-hub                       # machines from ~/.config/astrea/machines.json
//	astrea-hub --ws ws://100.x.y.z:7700/ws --token T   # a single machine
//	astrea-hub gateway               # OpenAI-compatible API over the fleet's models
//	astrea-hub update                # hub, panel and every agent to the latest release
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gateway":
			runGateway(os.Args[2:])
			return
		case "update":
			runUpdate(os.Args[2:])
			return
		case "--version", "-version", "version":
			fmt.Printf("astrea-hub %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
			return
		}
	}

	fs := flag.NewFlagSet("astrea-hub", flag.ExitOnError)
	cfgPath := fs.String("config", fleet.DefaultPath(), "machines file: {\"machines\":[{\"name\",\"ws\",\"token\"}]}")
	wsURL := fs.String("ws", os.Getenv("ASTREA_WS"), "single agent WebSocket URL (instead of --config), e.g. ws://100.x.y.z:7700/ws")
	token := fs.String("token", os.Getenv("ASTREA_TOKEN"), "bearer token for --ws (or ASTREA_TOKEN env)")
	name := fs.String("name", "main", "machine name for --ws")
	prefix := fs.String("prefix", "astrea.", "prefix prepended to each capability when exposed as an MCP tool")
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "directory for the cached capability lists")
	callTimeout := fs.Duration("call-timeout", 150*time.Second, "max wait per tool call (calls with timeout_sec wait timeout_sec+15s)")
	refresh := fs.Duration("refresh", 30*time.Second, "how often to re-sync the tool list with the agents (0 = never)")
	panelURL := fs.String("panel", orEnv("ASTREA_PANEL", "http://127.0.0.1:7780"), "astrea-panel URL, where fleet.events reads the notices")
	_ = fs.String("caps", "", "deprecated: derived from --ws")
	_ = fs.Parse(os.Args[1:])

	logger := log.New(os.Stderr, "astrea-hub ", log.LstdFlags)
	machines, err := loadFleet(*cfgPath, *wsURL, *token, *name)
	if err != nil {
		logger.Fatal(err)
	}
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "astrea",
		Title:   "Astrea — remote machine control",
		Version: version,
	}, &mcp.ServerOptions{Instructions: serverInstructions})

	h := newHub(server, fleet.New(machines), *prefix, *cacheDir, *callTimeout, logger)
	addEventsTool(server, *prefix, strings.TrimRight(*panelURL, "/"))
	h.loadCaches()
	_ = h.sync(ctx)
	if *refresh > 0 {
		go func() {
			for range time.Tick(*refresh) {
				_ = h.sync(ctx)
			}
		}()
	}

	logger.Printf("astrea-hub ready: %d tools across %d machine(s) %v", h.count(), len(machines), h.fleet.Names())
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("run: %v", err)
	}
}

// loadFleet picks the machines: --ws for a single agent, else the machines
// file, else the local agent on 127.0.0.1 (the historical default).
func loadFleet(cfgPath, ws, token, name string) ([]fleet.Machine, error) {
	if ws != "" {
		ms := []fleet.Machine{{Name: name, WS: ws, Token: token}}
		return ms, fleet.Validate(ms)
	}
	ms, err := fleet.Load(cfgPath)
	if errors.Is(err, fleet.ErrNoConfig) {
		return []fleet.Machine{{Name: name, WS: "ws://127.0.0.1:7700/ws", Token: token}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load machines: %w", err)
	}
	return ms, nil
}

func defaultCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "astrea")
	}
	return filepath.Join(os.TempDir(), "astrea")
}

// serverInstructions reach the AI when it connects.
const serverInstructions = `Astrea controls the owner's machines. The owner watches what you do on each one as a task list.

Every action that changes a machine (exec, fs.write, mouse/keyboard, jobs, services, tasks…) takes a "work" argument saying which task it belongs to:
- {"title": "...", "detail": "..."} opens a new task on that machine (closing the previous one) — any kind of work: "Install and configure Blender", "Run the test suite of project X", "Build the release of the app", "Clean up the Downloads folder";
- "continue" means it is part of the task already open there.
Report progress with work.update ("3/5") and close with work.end (done / failed / cancelled, plus a short summary of what was produced). Looking (fleet.status, sys.stats, screen.capture, fs.read…) needs no task.

When you wait for something long (a job, a download, a render), call fleet.events with wait_sec instead of polling; the owner may also wake you with an "Aviso de Astrea" message when something important happens.

Each PC has a memory: when you open a task on one you get what it has and what was done there. When you learn something worth knowing next time (where a program or model lives, how to start it, a quirk, a fix that worked), save it with memory.note — one short fact per note.

Working on a PC's screen — use the most exact tool that can do the step, in this order:
1. Open programs, files and folders directly, not by clicking icons: gui.exec with start explorer C:\path, start "" C:\file.docx or start chrome https://…. Make folders and files with exec/fs.write unless the owner wants to see it done on screen.
2. Windows (bring to front, minimize, maximize, restore, close): window.set by title or program; window.list says what is open.
3. Buttons, menus and fields of desktop apps (File Explorer, Office, Settings…): ui.find / ui.snapshot, then ui.act — exact labels and positions. The taskbar is ui.find with window "taskbar".
4. Web pages: browser.* (it reads and clicks the page itself).
5. Standard keyboard shortcuts when they are certain (key.press "ctrl+l" for File Explorer's address bar, "ctrl+shift+n" for a new folder…).
6. vision.locate only for what the tools above cannot see (games, canvases, apps drawn by hand, or ui.snapshot comes back empty). Describe the exact spot to click; it takes ~10 s on a 16 GB GPU and ~2 min on a small laptop GPU.
After any step that changes the screen, check the result (window.list, ui.find or screen.capture) before the next one — a window that is still opening is not there yet, and typing into the wrong place does damage.`

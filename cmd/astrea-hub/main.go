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
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "gateway" {
		runGateway(os.Args[2:])
		return
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
	}, nil)

	h := newHub(server, fleet.New(machines), *prefix, *cacheDir, *callTimeout, logger)
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

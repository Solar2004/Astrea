// Command astrea-hub is the MCP server that Hermes loads. It connects to one
// astrea-agent over the tailnet, mirrors the agent's capabilities as MCP tools,
// and forwards each tool call to the agent, returning the result (and any
// streamed output) as tool content.
//
// It is resilient to a down agent: capabilities are cached on disk from the
// last successful connect, so the MCP tool list is available immediately even
// if the agent is temporarily unreachable, and calls reconnect automatically.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	wsURL := flag.String("ws", envOr("ASTREA_WS", "ws://127.0.0.1:7700/ws"), "agent control-channel WebSocket URL")
	httpURL := flag.String("caps", envOr("ASTREA_CAPS", "http://127.0.0.1:7700/capabilities"), "agent capabilities URL")
	token := flag.String("token", os.Getenv("ASTREA_TOKEN"), "bearer token (or ASTREA_TOKEN env)")
	prefix := flag.String("prefix", "astrea.", "prefix prepended to each capability when exposed as an MCP tool")
	cacheDir := flag.String("cache-dir", defaultCacheDir(), "directory for the cached capability list")
	flag.Parse()

	logger := log.New(os.Stderr, "astrea-hub ", log.LstdFlags)
	ctx := context.Background()

	client := transport.NewReconnecting(*wsURL, *token)

	// Discover tools fast (short timeout) so we never block Hermes's MCP
	// discovery. Fall back to the on-disk cache if the agent is unreachable.
	cachePath := filepath.Join(*cacheDir, "capabilities.json")
	caps := discover(ctx, *httpURL, *token, cachePath, logger)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "astrea",
		Title:   "Astrea — remote machine control",
		Version: version,
	}, nil)

	for _, c := range caps {
		method := c.Name
		schema := c.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		server.AddTool(&mcp.Tool{
			Name:        *prefix + method,
			Description: c.Description,
			InputSchema: schema,
		}, makeHandler(client, method))
	}

	logger.Printf("astrea-hub ready: %d tools, agent=%s", len(caps), *wsURL)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("run: %v", err)
	}
}

// discover fetches capabilities with a short timeout, caching on success and
// falling back to the cache (or an empty list) on failure.
func discover(ctx context.Context, httpURL, token, cachePath string, logger *log.Logger) []transport.CapDescriptor {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	caps, err := transport.FetchCapabilities(dctx, httpURL, token)
	if err == nil && len(caps) > 0 {
		if data, e := json.MarshalIndent(caps, "", "  "); e == nil {
			_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
			_ = os.WriteFile(cachePath, data, 0o644)
		}
		return caps
	}
	logger.Printf("live discovery failed (%v); trying cache %s", err, cachePath)
	if data, e := os.ReadFile(cachePath); e == nil {
		var cached []transport.CapDescriptor
		if json.Unmarshal(data, &cached) == nil && len(cached) > 0 {
			logger.Printf("loaded %d tools from cache", len(cached))
			return cached
		}
	}
	logger.Printf("no cache available; starting with zero tools (will work once agent is reachable and hub restarts)")
	return nil
}

// makeHandler returns an MCP tool handler that forwards the raw arguments to
// the agent capability and renders the response as tool content.
func makeHandler(client *transport.ReconnectingClient, method string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args any = map[string]any{}
		if len(req.Params.Arguments) > 0 {
			args = json.RawMessage(req.Params.Arguments)
		}
		callCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		defer cancel()
		res, err := client.Call(callCtx, method, args)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcp.CallToolResult{Content: render(res)}, nil
	}
}

// render turns an agent CallResult into MCP content: streamed output first,
// then the structured result as pretty JSON.
func render(res *transport.CallResult) []mcp.Content {
	var out []mcp.Content
	if s := joinStreams(res.Streams); s != "" {
		out = append(out, &mcp.TextContent{Text: s})
	}
	if len(res.Result) > 0 && string(res.Result) != "null" {
		var pretty any
		if json.Unmarshal(res.Result, &pretty) == nil {
			b, _ := json.MarshalIndent(pretty, "", "  ")
			out = append(out, &mcp.TextContent{Text: string(b)})
		} else {
			out = append(out, &mcp.TextContent{Text: string(res.Result)})
		}
	}
	if len(out) == 0 {
		out = append(out, &mcp.TextContent{Text: "(no output)"})
	}
	return out
}

func joinStreams(chunks []protocol.StreamChunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Data)
	}
	return b.String()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "astrea")
	}
	return filepath.Join(os.TempDir(), "astrea")
}

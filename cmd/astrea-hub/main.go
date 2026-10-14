// Command astrea-hub is the MCP server that Hermes loads. It connects to one
// astrea-agent over the tailnet, mirrors the agent's capabilities as MCP tools,
// and forwards each tool call to the agent, returning the result (and any
// streamed output) as tool content.
//
// It is resilient to a down agent: capabilities are cached on disk from the
// last successful connect, so the MCP tool list is available immediately even
// if the agent is temporarily unreachable, and calls reconnect automatically.
// The tool list is kept in sync with the agent while running, so tasks the AI
// defines on the agent show up as new tools without restarting anything.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	callTimeout := flag.Duration("call-timeout", 150*time.Second, "max wait per tool call (calls with timeout_sec wait timeout_sec+15s)")
	refresh := flag.Duration("refresh", 30*time.Second, "how often to re-sync the tool list with the agent (0 = never)")
	flag.Parse()

	logger := log.New(os.Stderr, "astrea-hub ", log.LstdFlags)
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "astrea",
		Title:   "Astrea — remote machine control",
		Version: version,
	}, nil)

	h := &hub{
		server:      server,
		client:      transport.NewReconnecting(*wsURL, *token),
		capsURL:     *httpURL,
		token:       *token,
		prefix:      *prefix,
		cachePath:   filepath.Join(*cacheDir, "capabilities.json"),
		callTimeout: *callTimeout,
		logger:      logger,
		tools:       map[string]string{},
	}

	// Discover tools fast (short timeout) so we never block Hermes's MCP
	// discovery. Fall back to the on-disk cache if the agent is unreachable.
	if err := h.sync(ctx); err != nil {
		logger.Printf("live discovery failed (%v); trying cache %s", err, h.cachePath)
		if cached := loadCache(h.cachePath); len(cached) > 0 {
			h.apply(cached)
			logger.Printf("loaded %d tools from cache", len(cached))
		} else {
			logger.Printf("no cache available; tools will appear once the agent is reachable")
		}
	}
	if *refresh > 0 {
		go func() {
			for range time.Tick(*refresh) {
				_ = h.sync(ctx)
			}
		}()
	}

	logger.Printf("astrea-hub ready: %d tools, agent=%s", h.count(), *wsURL)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("run: %v", err)
	}
}

// hub mirrors one agent's capabilities as MCP tools.
type hub struct {
	server      *mcp.Server
	client      *transport.ReconnectingClient
	capsURL     string
	token       string
	prefix      string
	cachePath   string
	callTimeout time.Duration
	logger      *log.Logger

	mu    sync.Mutex
	tools map[string]string // capability name -> fingerprint of its descriptor
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tools)
}

// sync fetches the agent's capabilities, updates the MCP tool list and the
// on-disk cache. Adding/removing tools notifies the MCP client
// (tools/list_changed).
func (h *hub) sync(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	caps, err := transport.FetchCapabilities(dctx, h.capsURL, h.token)
	if err != nil {
		return err
	}
	if len(caps) == 0 {
		return fmt.Errorf("agent advertised no capabilities")
	}
	if h.apply(caps) {
		if data, e := json.MarshalIndent(caps, "", "  "); e == nil {
			_ = os.MkdirAll(filepath.Dir(h.cachePath), 0o755)
			_ = os.WriteFile(h.cachePath, data, 0o644)
		}
	}
	return nil
}

// apply makes the registered tools match caps. Reports whether anything
// changed.
func (h *hub) apply(caps []transport.CapDescriptor) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	changed := false
	for _, c := range caps {
		seen[c.Name] = true
		fp := c.Description + "\x00" + string(c.InputSchema)
		if h.tools[c.Name] == fp {
			continue
		}
		schema := c.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		h.server.AddTool(&mcp.Tool{
			Name:        h.prefix + c.Name,
			Description: c.Description,
			InputSchema: schema,
		}, h.handler(c.Name))
		h.tools[c.Name] = fp
		changed = true
	}
	var gone []string
	for name := range h.tools {
		if !seen[name] {
			gone = append(gone, h.prefix+name)
			delete(h.tools, name)
		}
	}
	if len(gone) > 0 {
		h.server.RemoveTools(gone...)
		changed = true
	}
	return changed
}

// handler returns an MCP tool handler that forwards the raw arguments to the
// agent capability and renders the response as tool content.
func (h *hub) handler(method string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args any = map[string]any{}
		raw := json.RawMessage(req.Params.Arguments)
		if len(raw) > 0 {
			args = raw
		}
		callCtx, cancel := context.WithTimeout(ctx, h.timeoutFor(raw))
		defer cancel()
		res, err := h.client.Call(callCtx, method, args)
		if err == nil && (method == "task.define" || method == "task.delete") {
			// New/removed tasks are new/removed tools: publish them now.
			_ = h.sync(ctx)
		}
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcp.CallToolResult{Content: render(res)}, nil
	}
}

// timeoutFor gives calls that declare their own timeout_sec enough time to
// finish on the agent before the hub gives up (and cancels them).
func (h *hub) timeoutFor(args json.RawMessage) time.Duration {
	var p struct {
		TimeoutSec int `json:"timeout_sec"`
	}
	if json.Unmarshal(args, &p) == nil && p.TimeoutSec > 0 {
		if d := time.Duration(p.TimeoutSec)*time.Second + 15*time.Second; d > h.callTimeout {
			return d
		}
	}
	return h.callTimeout
}

func loadCache(path string) []transport.CapDescriptor {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cached []transport.CapDescriptor
	if json.Unmarshal(data, &cached) != nil {
		return nil
	}
	return cached
}

// render turns an agent CallResult into MCP content: streamed output first,
// then images (any top-level "images" array in the result) as native image
// content, then the rest of the result as pretty JSON.
func render(res *transport.CallResult) []mcp.Content {
	var out []mcp.Content
	if s := joinStreams(res.Streams); s != "" {
		out = append(out, &mcp.TextContent{Text: s})
	}
	if len(res.Result) > 0 && string(res.Result) != "null" {
		var obj map[string]json.RawMessage
		if json.Unmarshal(res.Result, &obj) == nil {
			images, rest := extractImages(obj)
			out = append(out, images...)
			if len(rest) > 0 {
				out = append(out, &mcp.TextContent{Text: prettyJSON(rest)})
			}
		} else {
			out = append(out, &mcp.TextContent{Text: prettyJSON(res.Result)})
		}
	}
	if len(out) == 0 {
		out = append(out, &mcp.TextContent{Text: "(no output)"})
	}
	return out
}

// extractImages pulls decodable entries out of obj["images"], returning them
// as MCP image content and obj without them.
func extractImages(obj map[string]json.RawMessage) ([]mcp.Content, map[string]json.RawMessage) {
	raw, ok := obj["images"]
	if !ok {
		return nil, obj
	}
	var imgs []struct {
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
	}
	if json.Unmarshal(raw, &imgs) != nil {
		return nil, obj
	}
	var out []mcp.Content
	for _, im := range imgs {
		data, err := base64.StdEncoding.DecodeString(im.Data)
		if err != nil || !strings.HasPrefix(im.MimeType, "image/") {
			continue
		}
		out = append(out, &mcp.ImageContent{MIMEType: im.MimeType, Data: data})
	}
	delete(obj, "images")
	return out, obj
}

func prettyJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") != nil {
		return string(b)
	}
	return buf.String()
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

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fleetStatusTool is served by the hub itself, not by an agent.
const fleetStatusTool = "fleet.status"

// hub mirrors the fleet's capabilities as MCP tools.
type hub struct {
	server      *mcp.Server
	fleet       *fleet.Fleet
	prefix      string
	cacheDir    string
	callTimeout time.Duration
	logger      *log.Logger

	mu    sync.Mutex
	caps  map[string][]transport.CapDescriptor // machine -> last known capabilities
	tools map[string]string                    // capability -> fingerprint of the registered tool
	owner map[string][]string                  // capability -> machines that have it
}

func newHub(server *mcp.Server, f *fleet.Fleet, prefix, cacheDir string, callTimeout time.Duration, logger *log.Logger) *hub {
	h := &hub{
		server: server, fleet: f, prefix: prefix, cacheDir: cacheDir, callTimeout: callTimeout, logger: logger,
		caps: map[string][]transport.CapDescriptor{}, tools: map[string]string{}, owner: map[string][]string{},
	}
	server.AddTool(&mcp.Tool{
		Name: prefix + fleetStatusTool,
		Description: "Snapshot of every controlled machine: online, OS, CPU/RAM use, GPUs with free VRAM, " +
			"whether a user is active (and the foreground app, e.g. a game), supervised services, and the task the AI has declared on it (work). " +
			"Use it to choose which machine should run a job or a model.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		st := h.fleet.StatusAll(ctx)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: prettyJSON(st)}}}, nil
	})
	return h
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tools) + 1
}

func (h *hub) cachePath(machine string) string {
	return filepath.Join(h.cacheDir, "capabilities-"+machine+".json")
}

// loadCaches seeds every machine's capabilities from disk, so tools are listed
// at once even if agents are unreachable.
func (h *hub) loadCaches() {
	h.mu.Lock()
	for _, n := range h.fleet.Nodes {
		data, err := os.ReadFile(h.cachePath(n.Name))
		if err != nil {
			continue
		}
		var c []transport.CapDescriptor
		if json.Unmarshal(data, &c) == nil && len(c) > 0 {
			h.caps[n.Name] = c
		}
	}
	h.mu.Unlock()
	h.apply()
}

// sync refreshes every reachable machine's capabilities (keeping the last
// known list for unreachable ones) and updates the MCP tool list, which
// notifies the client (tools/list_changed) when it changes.
func (h *hub) sync(ctx context.Context) error {
	var mu sync.Mutex
	var failed []string
	h.fleet.Each(func(n *fleet.Node) {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		caps, err := transport.FetchCapabilities(dctx, n.CapsURL(), n.Token)
		if err == nil && len(caps) == 0 {
			err = fmt.Errorf("no capabilities advertised")
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			failed = append(failed, n.Name+": "+err.Error())
			return
		}
		h.mu.Lock()
		h.caps[n.Name] = caps
		h.mu.Unlock()
		if data, e := json.MarshalIndent(caps, "", "  "); e == nil {
			_ = os.MkdirAll(h.cacheDir, 0o755)
			_ = os.WriteFile(h.cachePath(n.Name), data, 0o644)
		}
	})
	h.apply()
	if len(failed) > 0 {
		return fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return nil
}

// apply makes the registered tools the union of all machines' capabilities.
func (h *hub) apply() {
	h.mu.Lock()
	defer h.mu.Unlock()
	multi := len(h.fleet.Nodes) > 1
	desc := map[string]transport.CapDescriptor{}
	owner := map[string][]string{}
	for _, name := range h.fleet.Names() { // config order: first machine's descriptor wins
		for _, c := range h.caps[name] {
			if c.Name == fleetStatusTool || strings.HasPrefix(c.Name, "panel.") {
				continue // panel.*: the owner's controls, not for the AI
			}
			if _, ok := desc[c.Name]; !ok {
				desc[c.Name] = c
			}
			owner[c.Name] = append(owner[c.Name], name)
		}
	}
	for capName, c := range desc {
		machines := owner[capName]
		schema := withMachineParam(c.InputSchema, machines, multi)
		if needsWork(capName) {
			schema = withWorkParam(schema)
		}
		description := c.Description
		if multi {
			description += " Machines: " + strings.Join(machines, ", ") + "."
		}
		fp := description + "\x00" + string(schema)
		if h.tools[capName] == fp {
			continue
		}
		h.server.AddTool(&mcp.Tool{Name: h.prefix + capName, Description: description, InputSchema: schema}, h.handler(capName))
		h.tools[capName] = fp
	}
	var gone []string
	for capName := range h.tools {
		if _, ok := desc[capName]; !ok {
			gone = append(gone, h.prefix+capName)
			delete(h.tools, capName)
		}
	}
	if len(gone) > 0 {
		h.server.RemoveTools(gone...)
	}
	h.owner = owner
}

// withMachineParam adds a "machine" property to a capability's input schema
// when the fleet has several machines; it is required only if more than one
// of them offers the capability.
func withMachineParam(schema json.RawMessage, machines []string, multi bool) json.RawMessage {
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	if !multi {
		return schema
	}
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["machine"] = map[string]any{
		"type":        "string",
		"enum":        machines,
		"description": "Which machine to run on. See fleet.status.",
	}
	s["properties"] = props
	if len(machines) > 1 {
		req, _ := s["required"].([]any)
		s["required"] = append(req, "machine")
	}
	out, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return out
}

// readOnly are the capabilities that only look: they need no declared work.
var readOnly = map[string]bool{
	"admin.audit": true, "admin.status": true, "deps.check": true, "endpoint.list": true,
	"fs.list": true, "fs.read": true, "job.logs": true, "job.status": true,
	"net.download.status": true, "proc.list": true, "screen.capture": true,
	"service.logs": true, "service.status": true, "sys.idle": true, "sys.info": true,
	"sys.stats": true, "task.get": true, "task.list": true,
}

// needsWork reports whether calling capName must say which task it is part
// of. Everything that changes the machine does, except declaring work itself
// and the owner's controls (kill switch, updates).
func needsWork(capName string) bool {
	switch {
	case readOnly[capName], strings.HasPrefix(capName, "work."),
		strings.HasPrefix(capName, "admin."), strings.HasPrefix(capName, "agent."),
		strings.HasPrefix(capName, "memory."):
		return false
	}
	return true
}

const workParamDesc = `Which task this action is part of — shown to the machine's owner and kept in the audit. ` +
	`"continue" if it belongs to the task already open on this machine; ` +
	`for something new: {"title": "what, in a few words", "detail": "why / for what"} (closes the previous task). ` +
	`Report progress with work.update and close with work.end.`

// withWorkParam adds the required "work" argument to a capability's schema.
func withWorkParam(schema json.RawMessage) json.RawMessage {
	var s map[string]any
	if len(schema) == 0 || json.Unmarshal(schema, &s) != nil {
		s = map[string]any{"type": "object"}
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["work"] = map[string]any{
		"description": workParamDesc,
		"anyOf": []any{
			map[string]any{"type": "string", "enum": []string{"continue"}},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":  map[string]any{"type": "string"},
					"detail": map[string]any{"type": "string"},
				},
				"required": []string{"title"},
			},
		},
	}
	s["properties"] = props
	req, _ := s["required"].([]any)
	s["required"] = append(req, "work")
	out, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return out
}

// route extracts the "machine" argument, returning the target node and the
// arguments to forward (raw values untouched, so integers stay integers).
// The "work" argument is removed too and returned apart.
func (h *hub) route(capName string, raw json.RawMessage) (*fleet.Node, json.RawMessage, json.RawMessage, error) {
	args := map[string]json.RawMessage{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, nil, nil, fmt.Errorf("arguments must be a JSON object: %v", err)
		}
	}
	workSpec := args["work"]
	delete(args, "work")
	if needsWork(capName) && (len(workSpec) == 0 || string(workSpec) == "null") {
		return nil, nil, nil, fmt.Errorf(`missing "work": say which task this is part of — "continue" for the task already open on that machine, or {"title": "...", "detail": "..."} for a new one`)
	}
	var machine string
	if m, ok := args["machine"]; ok {
		_ = json.Unmarshal(m, &machine)
		delete(args, "machine")
	}
	h.mu.Lock()
	owners := h.owner[capName]
	h.mu.Unlock()
	switch {
	case machine != "":
	case len(h.fleet.Nodes) == 1:
		machine = h.fleet.Nodes[0].Name
	case len(owners) == 1:
		machine = owners[0]
	default:
		return nil, nil, nil, fmt.Errorf("pass machine: one of %v (see %s%s)", owners, h.prefix, fleetStatusTool)
	}
	n := h.fleet.Get(machine)
	if n == nil {
		return nil, nil, nil, fmt.Errorf("unknown machine %q (known: %v)", machine, h.fleet.Names())
	}
	fwd, err := json.Marshal(args)
	if !needsWork(capName) {
		workSpec = nil
	}
	return n, fwd, workSpec, err
}

// handler forwards a tool call to the chosen machine and renders the result.
func (h *hub) handler(capName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		n, args, workSpec, err := h.route(capName, json.RawMessage(req.Params.Arguments))
		if err != nil {
			return errorResult(err), nil
		}
		if workSpec != nil {
			ctx = protocol.WithWorkSpec(ctx, workSpec)
		}
		callCtx, cancel := context.WithTimeout(ctx, h.timeoutFor(args))
		defer cancel()
		res, err := n.Client.Call(callCtx, capName, args)
		if err == nil && (capName == "task.define" || capName == "task.delete") {
			// New/removed tasks are new/removed tools: publish them now.
			_ = h.sync(ctx)
		}
		if err != nil {
			return errorResult(fmt.Errorf("%s: %w", n.Name, err)), nil
		}
		content := render(res)
		if capName == "work.start" || bytes.HasPrefix(bytes.TrimSpace(workSpec), []byte("{")) {
			content = append(content, h.memory(ctx, n)...)
		}
		return &mcp.CallToolResult{Content: content}, nil
	}
}

// memory is what the AI should know about a machine when it opens a new task
// there (memory.get summary); nothing if the agent is too old to have it.
func (h *hub) memory(ctx context.Context, n *fleet.Node) []mcp.Content {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := n.Client.Call(ctx, "memory.get", json.RawMessage(`{}`))
	if err != nil {
		return nil
	}
	var m struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(res.Result, &m) != nil || m.Summary == "" {
		return nil
	}
	return []mcp.Content{&mcp.TextContent{Text: "Memory of " + n.Name + " (what is there and what was done before):\n" + m.Summary}}
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
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

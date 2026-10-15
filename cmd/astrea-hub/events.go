package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fleetEventsTool reads the fleet notices recorded by astrea-panel, which
// watches every machine (GET /api/events). Served by the hub itself.
const fleetEventsTool = "fleet.events"

func addEventsTool(server *mcp.Server, prefix, panelURL string) {
	client := &http.Client{Timeout: 140 * time.Second}
	server.AddTool(&mcp.Tool{
		Name: prefix + fleetEventsTool,
		Description: "What happened on the machines: task/job/download finished or failed, service crashed, machine went online/offline, " +
			"emergency brake, someone sat down at a PC or left it free, hot GPU, full disk. " +
			"Each event has a seq; pass the last one as since to get only newer ones. " +
			"wait_sec (max 120) blocks until something new happens — use it instead of polling when you are waiting for a long job.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since": {"type": "integer", "description": "Only events after this seq (0 = the recent ones)."},
				"wait_sec": {"type": "integer", "description": "Wait up to this long for a new event (max 120)."},
				"machine": {"type": "string", "description": "Only this machine."}
			}
		}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Since   int64  `json:"since"`
			WaitSec int    `json:"wait_sec"`
			Machine string `json:"machine"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errorResult(err), nil
			}
		}
		q := url.Values{"since": {strconv.FormatInt(args.Since, 10)}, "wait": {strconv.Itoa(min(max(args.WaitSec, 0), 120))}}
		if args.Machine != "" {
			q.Set("machine", args.Machine)
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, panelURL+"/api/events?"+q.Encode(), nil)
		if err != nil {
			return errorResult(err), nil
		}
		res, err := client.Do(hreq)
		if err != nil {
			return errorResult(fmt.Errorf("astrea-panel is not running at %s (it records the events): %w", panelURL, err)), nil
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if res.StatusCode != http.StatusOK {
			return errorResult(fmt.Errorf("astrea-panel: HTTP %d: %s", res.StatusCode, body)), nil
		}
		var out any
		if err := json.Unmarshal(body, &out); err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: prettyJSON(out)}}}, nil
	})
}

func orEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

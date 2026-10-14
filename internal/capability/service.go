package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/supervise"
)

// ServiceStart defines and runs a supervised long-lived process, optionally
// publishing it as a dynamic endpoint.
type ServiceStart struct {
	Svcs *supervise.Services
	Dyn  *endpoint.Manager
}

func (ServiceStart) Name() string { return "service.start" }

func (ServiceStart) Describe() Descriptor {
	return Descriptor{
		Name: "service.start",
		Description: "Run a long-lived server under supervision (llama-server, ComfyUI, ollama serve, a custom API…): restarted if it crashes, " +
			"started again when the agent boots (autostart), output logged to disk. Calling it again with the same name replaces the service; " +
			"with only a name it restarts the existing one. " +
			"ready makes the call wait until the server answers (url must return 200; e.g. llama-server http://127.0.0.1:8080/health). " +
			"expose publishes it at /dyn/<endpoint> on the agent, so the hub machine can reach it over the tailnet " +
			"(an OpenAI-compatible server becomes http://<agent>:7700/dyn/<endpoint>/v1/chat/completions).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Service id: letters, digits, '.', '_' or '-'."},
				"command": {"type": "string", "description": "Command line. Prefer launching the server binary directly so stop kills it cleanly."},
				"shell": {"type": "string", "enum": ["default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"]},
				"cwd": {"type": "string"},
				"env": {"type": "object", "additionalProperties": {"type": "string"}},
				"restart": {"type": "string", "enum": ["on-failure", "always", "never"], "description": "Default on-failure (exponential backoff up to 60s)."},
				"autostart": {"type": "boolean", "description": "Start again when the agent starts. Default true."},
				"ready": {"type": "object", "properties": {"url": {"type": "string"}, "port": {"type": "integer"}}, "description": "Readiness probe: an http url that must return 200, or a local TCP port."},
				"wait_ready_sec": {"type": "integer", "description": "How long to wait for ready before returning (max 120, default 60). Big models can take longer: then poll service.status."},
				"expose": {"type": "object", "properties": {"endpoint": {"type": "string"}, "port": {"type": "integer"}}, "required": ["endpoint", "port"], "description": "Publish 127.0.0.1:<port> as /dyn/<endpoint>."}
			},
			"required": ["name"]
		}`),
	}
}

func (c ServiceStart) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		supervise.ServiceSpec
		Autostart    *bool `json:"autostart"`
		WaitReadySec *int  `json:"wait_ready_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	spec := req.ServiceSpec
	spec.Autostart = req.Autostart == nil || *req.Autostart
	if e := spec.Expose; e != nil {
		if err := supervise.ValidName(e.Endpoint); err != nil {
			return nil, badRequest(fmt.Errorf("expose.endpoint: %w", err))
		}
		if e.Port <= 0 || e.Port > 65535 {
			return nil, badRequest(fmt.Errorf("expose.port must be 1-65535"))
		}
	}

	st, err := c.Svcs.Start(spec)
	if err != nil {
		return nil, badRequest(err)
	}
	out := map[string]any{}
	if e := st.Expose; e != nil && c.Dyn != nil {
		target := "http://127.0.0.1:" + strconv.Itoa(e.Port)
		if err := c.Dyn.Create(&endpoint.Spec{Name: e.Endpoint, Kind: endpoint.KindProxy, Target: target}); err != nil {
			out["expose_error"] = err.Error()
		} else {
			out["endpoint"] = "/dyn/" + e.Endpoint
		}
	}

	wait := 60
	if req.WaitReadySec != nil {
		wait = *req.WaitReadySec
	}
	if wait > 0 {
		ready, why := c.Svcs.WaitReady(ctx, st.Name, waitDur(wait))
		out["ready"] = ready
		if !ready {
			out["not_ready_reason"] = why
		}
	}
	st, _ = c.Svcs.Status(st.Name)
	out["service"] = st
	if ready, _ := out["ready"].(bool); !ready {
		out["log_tail"] = tail(c.Svcs.Log(st.Name, 0, 30, 0))
	}
	return out, nil
}

// ServiceStop stops (and optionally removes) a service.
type ServiceStop struct {
	Svcs *supervise.Services
	Dyn  *endpoint.Manager
}

func (ServiceStop) Name() string { return "service.stop" }

func (ServiceStop) Describe() Descriptor {
	return Descriptor{
		Name:        "service.stop",
		Description: "Stop a service and its whole process tree. It stays defined (autostart off) so service.start {name} restarts it; remove=true deletes it and its exposed endpoint.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"remove": {"type": "boolean"}
			},
			"required": ["name"]
		}`),
	}
}

func (c ServiceStop) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name   string `json:"name"`
		Remove bool   `json:"remove"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	st, err := c.Svcs.Stop(req.Name, req.Remove)
	if err != nil {
		return nil, badRequest(err)
	}
	if req.Remove && st.Expose != nil && c.Dyn != nil {
		c.Dyn.Delete(st.Expose.Endpoint)
	}
	return map[string]any{"service": st, "removed": req.Remove}, nil
}

// ServiceStatus reports one or all services.
type ServiceStatus struct{ Svcs *supervise.Services }

func (ServiceStatus) Name() string { return "service.status" }

func (ServiceStatus) Describe() Descriptor {
	return Descriptor{
		Name:        "service.status",
		Description: "State of a service (starting|running|backoff|exited|stopped|failed), pid, uptime, restarts, last exit code and last log lines. Without name, list all services.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"name": {"type": "string"}}
		}`),
	}
}

func (c ServiceStatus) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return map[string]any{"services": c.Svcs.List()}, nil
	}
	st, err := c.Svcs.Status(req.Name)
	if err != nil {
		return nil, badRequest(err)
	}
	return map[string]any{"service": st, "log_tail": tail(c.Svcs.Log(req.Name, 0, 20, 0))}, nil
}

// ServiceLogs pages through a service's output.
type ServiceLogs struct{ Svcs *supervise.Services }

func (ServiceLogs) Name() string { return "service.logs" }

func (ServiceLogs) Describe() Descriptor {
	return Descriptor{
		Name:        "service.logs",
		Description: "Read a service's combined stdout/stderr log (all runs, each starting with a '=== start' line), by byte offset or as the last N lines.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},` + logArgsSchema + `
			},
			"required": ["name"]
		}`),
	}
}

func (c ServiceLogs) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
		logRequest
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	chunk, err := c.Svcs.Log(req.Name, req.Offset, req.TailLines, req.MaxBytes)
	if err != nil {
		return nil, badRequest(err)
	}
	return chunk, nil
}

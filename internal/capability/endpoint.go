package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// EndpointCreate mounts a dynamic HTTP endpoint on the controlled machine at
// /dyn/<name>, enabling Hermes to build internal APIs in real time.
type EndpointCreate struct{ Mgr *endpoint.Manager }

func (EndpointCreate) Name() string { return "endpoint.create" }

func (EndpointCreate) Describe() Descriptor {
	return Descriptor{
		Name: "endpoint.create",
		Description: "Mount a dynamic HTTP endpoint at /dyn/<name> on the controlled machine. " +
			"kind=exec runs a shell command (request body on stdin, stdout is the response); " +
			"kind=proxy reverse-proxies to a local target URL (e.g. a local AI server); " +
			"kind=static returns a fixed body.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Route name; served at /dyn/<name>."},
				"kind": {"type": "string", "enum": ["exec", "proxy", "static"]},
				"command": {"type": "string", "description": "exec: shell command. Request body arrives on stdin; query string in $ASTREA_QUERY."},
				"shell": {"type": "string", "enum": ["default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"], "description": "exec: shell for command."},
				"target": {"type": "string", "description": "proxy: base URL, e.g. http://127.0.0.1:11434"},
				"body": {"type": "string", "description": "static: response body."},
				"content_type": {"type": "string", "description": "static: Content-Type header."}
			},
			"required": ["name", "kind"]
		}`),
	}
}

func (e EndpointCreate) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var spec endpoint.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if err := e.Mgr.Create(&spec); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	return map[string]any{"name": spec.Name, "path": "/dyn/" + spec.Name, "kind": spec.Kind}, nil
}

// EndpointList lists the currently mounted dynamic endpoints.
type EndpointList struct{ Mgr *endpoint.Manager }

func (EndpointList) Name() string { return "endpoint.list" }

func (EndpointList) Describe() Descriptor {
	return Descriptor{
		Name:        "endpoint.list",
		Description: "List the dynamic HTTP endpoints currently mounted on the controlled machine.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (e EndpointList) Invoke(_ context.Context, _ json.RawMessage, _ Emit) (any, error) {
	return map[string]any{"endpoints": e.Mgr.List()}, nil
}

// EndpointDelete removes a dynamic endpoint.
type EndpointDelete struct{ Mgr *endpoint.Manager }

func (EndpointDelete) Name() string { return "endpoint.delete" }

func (EndpointDelete) Describe() Descriptor {
	return Descriptor{
		Name:        "endpoint.delete",
		Description: "Remove a dynamic HTTP endpoint from the controlled machine.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"name": {"type": "string"}},
			"required": ["name"]
		}`),
	}
}

func (e EndpointDelete) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Name == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "name is required"}
	}
	return map[string]any{"name": req.Name, "deleted": e.Mgr.Delete(req.Name)}, nil
}

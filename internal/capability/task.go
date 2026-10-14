package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/task"
)

// RegisterTasks registers the task.* management capabilities plus one
// "task.<name>" capability per stored task, so every task the AI defines is
// advertised (and exposed by the hub as an MCP tool) like a built-in.
func RegisterTasks(r *Registry, m *task.Manager) {
	r.Register(TaskDefine{Mgr: m, Reg: r})
	r.Register(TaskList{Mgr: m})
	r.Register(TaskGet{Mgr: m})
	r.Register(TaskDelete{Mgr: m, Reg: r})
	r.Register(TaskRun{Mgr: m})
	for _, s := range m.List() {
		r.Register(taskTool{mgr: m, spec: s})
	}
}

// taskTool is the dynamic capability for one stored task.
type taskTool struct {
	mgr  *task.Manager
	spec *task.Spec
}

func (t taskTool) Name() string { return "task." + t.spec.Name }

func (t taskTool) Describe() Descriptor {
	schema := t.spec.Params
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type": "object", "properties": {}}`)
	}
	desc := t.spec.Description
	if desc == "" {
		desc = "AI-defined " + string(t.spec.Kind) + " task."
	}
	return Descriptor{Name: t.Name(), Description: "[task] " + desc, InputSchema: schema}
}

func (t taskTool) Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error) {
	return t.mgr.Run(ctx, t.spec.Name, payload, task.Emit(emit))
}

// --- task.define ---

// TaskDefine creates or replaces a task and publishes it as task.<name>.
type TaskDefine struct {
	Mgr *task.Manager
	Reg *Registry
}

func (TaskDefine) Name() string { return "task.define" }

func (TaskDefine) Describe() Descriptor {
	return Descriptor{
		Name: "task.define",
		Description: "Create or replace a reusable automation on the controlled machine. It immediately becomes " +
			"its own tool, task.<name>, taking the arguments described by 'params'. " +
			"kind=script: a script file run by 'shell'; arguments arrive as env vars ASTREA_ARG_<NAME> " +
			"(uppercased), as JSON in $ASTREA_ARGS and on stdin. Stdout/stderr stream back. To return " +
			"structured data, write JSON to the file named by $ASTREA_RESULT; a top-level " +
			"\"images\": [{\"mime_type\",\"data\" (base64)}] in it is shown as images. " +
			"kind=macro: 'steps' run in order, each {\"call\": <capability, e.g. exec, screen.capture, " +
			"mouse.move, another task.x, or the built-in sleep {\"ms\"}>, \"args\": {...}}; " +
			"\"{{param}}\" in step args is replaced with the task argument. " +
			"Use script for logic and data, macro to drive the GUI (open app, wait, click, capture).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Letters, digits, _ or -. Exposed as task.<name>."},
				"description": {"type": "string", "description": "What the task does; shown as the tool description."},
				"params": {"type": "object", "description": "JSON schema (type object) of the task arguments. Supports default and required."},
				"kind": {"type": "string", "enum": ["script", "macro"]},
				"shell": {"type": "string", "enum": ["default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"], "description": "script: interpreter. Prefer powershell on Windows."},
				"script": {"type": "string", "description": "script: full script source."},
				"cwd": {"type": "string", "description": "script: working directory."},
				"steps": {
					"type": "array",
					"description": "macro: steps to run in order.",
					"items": {
						"type": "object",
						"properties": {
							"call": {"type": "string"},
							"args": {"type": "object"},
							"continue_on_error": {"type": "boolean"}
						},
						"required": ["call"]
					}
				},
				"timeout_sec": {"type": "integer", "description": "Max run time. Default 300."}
			},
			"required": ["name", "kind"]
		}`),
	}
}

func (d TaskDefine) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var spec task.Spec
	if err := json.Unmarshal(payload, &spec); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if err := d.Mgr.Define(&spec); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	tool := taskTool{mgr: d.Mgr, spec: &spec}
	d.Reg.Register(tool)
	return map[string]any{"name": spec.Name, "tool": tool.Name(), "kind": spec.Kind}, nil
}

// --- task.list ---

// TaskList lists the stored tasks.
type TaskList struct{ Mgr *task.Manager }

func (TaskList) Name() string { return "task.list" }

func (TaskList) Describe() Descriptor {
	return Descriptor{
		Name:        "task.list",
		Description: "List the automations (tasks) defined on the controlled machine.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (l TaskList) Invoke(_ context.Context, _ json.RawMessage, _ Emit) (any, error) {
	type summary struct {
		Name        string    `json:"name"`
		Kind        task.Kind `json:"kind"`
		Description string    `json:"description,omitempty"`
		Updated     string    `json:"updated,omitempty"`
	}
	out := []summary{}
	for _, s := range l.Mgr.List() {
		out = append(out, summary{s.Name, s.Kind, s.Description, s.Updated})
	}
	return map[string]any{"tasks": out}, nil
}

// --- task.get ---

// TaskGet returns a task's full definition, so the AI can inspect or edit it.
type TaskGet struct{ Mgr *task.Manager }

func (TaskGet) Name() string { return "task.get" }

func (TaskGet) Describe() Descriptor {
	return Descriptor{
		Name:        "task.get",
		Description: "Return the full definition (script or steps) of a task, e.g. to fix or extend it with task.define.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`),
	}
}

func (g TaskGet) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	name, err := nameArg(payload)
	if err != nil {
		return nil, err
	}
	s, ok := g.Mgr.Get(name)
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "no such task " + name}
	}
	return s, nil
}

// --- task.delete ---

// TaskDelete removes a task and its task.<name> capability.
type TaskDelete struct {
	Mgr *task.Manager
	Reg *Registry
}

func (TaskDelete) Name() string { return "task.delete" }

func (TaskDelete) Describe() Descriptor {
	return Descriptor{
		Name:        "task.delete",
		Description: "Delete a task (and its task.<name> tool) from the controlled machine.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`),
	}
}

func (d TaskDelete) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	name, err := nameArg(payload)
	if err != nil {
		return nil, err
	}
	ok := d.Mgr.Delete(name)
	if ok {
		d.Reg.Unregister("task." + name)
	}
	return map[string]any{"name": name, "deleted": ok}, nil
}

// --- task.run ---

// TaskRun runs a task by name. Equivalent to calling task.<name>; useful
// before the MCP client has refreshed its tool list.
type TaskRun struct{ Mgr *task.Manager }

func (TaskRun) Name() string { return "task.run" }

func (TaskRun) Describe() Descriptor {
	return Descriptor{
		Name:        "task.run",
		Description: "Run a task by name with arguments. Same as calling its task.<name> tool.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"name": {"type": "string"}, "args": {"type": "object"}},
			"required": ["name"]
		}`),
	}
}

func (t TaskRun) Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error) {
	var req struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Name == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "name is required"}
	}
	return t.Mgr.Run(ctx, req.Name, req.Args, task.Emit(emit))
}

func nameArg(payload json.RawMessage) (string, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Name == "" {
		return "", &protocol.Error{Code: protocol.CodeBadRequest, Message: "name is required"}
	}
	return req.Name, nil
}

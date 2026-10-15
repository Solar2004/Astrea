package capability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/work"
)

// RegisterWork registers the work.* capabilities: declaring, reporting and
// reading the tasks the AI works on here. auditPath ("" = disabled) feeds
// work.get with the actions each task actually ran.
func RegisterWork(r *Registry, s *work.Store, auditPath string) {
	r.Register(WorkStart{Store: s})
	r.Register(WorkUpdate{Store: s})
	r.Register(WorkEnd{Store: s})
	r.Register(WorkList{Store: s})
	r.Register(WorkGet{Store: s, AuditPath: auditPath})
}

// WorkStart opens a task explicitly.
type WorkStart struct{ Store *work.Store }

func (WorkStart) Name() string { return "work.start" }

func (WorkStart) Describe() Descriptor {
	return Descriptor{
		Name: "work.start",
		Description: "Declare the task you are starting on this machine (shown to the owner, kept in the audit). " +
			"Closes the previous task as replaced. You can also open a task inline by passing " +
			`work {"title", "detail"} to any action.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "what you are doing, a few words (e.g. \"Generate 5 videos with MiniMax\")"},
				"detail": {"type": "string", "description": "why / for whom / what done looks like"}
			},
			"required": ["title"]
		}`),
	}
}

func (w WorkStart) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Title, Detail string }
	_ = json.Unmarshal(payload, &req)
	if strings.TrimSpace(req.Title) == "" {
		return nil, badRequest(errors.New(`"title" is required: what you are doing, in a few words`))
	}
	return w.Store.Start(req.Title, req.Detail), nil
}

// WorkUpdate reports progress on the current task.
type WorkUpdate struct{ Store *work.Store }

func (WorkUpdate) Name() string { return "work.update" }

func (WorkUpdate) Describe() Descriptor {
	return Descriptor{
		Name:        "work.update",
		Description: "Report progress on this machine's current task: a short note and/or a progress figure (\"3/5\", \"60%\").",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"note": {"type": "string"},
				"progress": {"type": "string", "description": "e.g. \"3/5\" or \"60%\""}
			}
		}`),
	}
}

func (w WorkUpdate) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Note, Progress string }
	_ = json.Unmarshal(payload, &req)
	t, err := w.Store.Update(req.Note, req.Progress)
	if err != nil {
		return nil, badRequest(err)
	}
	return t, nil
}

// WorkEnd closes the current task.
type WorkEnd struct{ Store *work.Store }

func (WorkEnd) Name() string { return "work.end" }

func (WorkEnd) Describe() Descriptor {
	return Descriptor{
		Name:        "work.end",
		Description: "Close this machine's current task with its outcome and a short summary of what was done (files produced, results, what failed).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"status": {"type": "string", "enum": ["done", "failed", "cancelled"], "description": "default done"},
				"summary": {"type": "string"},
				"id": {"type": "string", "description": "optional: fail unless this is still the current task"}
			}
		}`),
	}
}

func (w WorkEnd) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ ID, Status, Summary string }
	_ = json.Unmarshal(payload, &req)
	t, err := w.Store.End(req.ID, req.Status, req.Summary)
	if err != nil {
		return nil, badRequest(err)
	}
	return t, nil
}

// WorkList lists recent tasks, newest first, plus the current one.
type WorkList struct{ Store *work.Store }

func (WorkList) Name() string { return "work.list" }

func (WorkList) Describe() Descriptor {
	return Descriptor{
		Name:        "work.list",
		Description: "This machine's current task and the recent ones (newest first): title, status, progress, duration, number of actions.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"limit": {"type": "integer", "description": "default 20, max 300"}}
		}`),
	}
}

func (w WorkList) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Limit int }
	_ = json.Unmarshal(payload, &req)
	if req.Limit <= 0 {
		req.Limit = 20
	}
	out := map[string]any{"tasks": w.Store.List(req.Limit)}
	if c, ok := w.Store.Current(); ok {
		out["current"] = c
	}
	return out, nil
}

// WorkGet returns one task with the actions that ran under it.
type WorkGet struct {
	Store     *work.Store
	AuditPath string
}

func (WorkGet) Name() string { return "work.get" }

func (WorkGet) Describe() Descriptor {
	return Descriptor{
		Name:        "work.get",
		Description: "One task in full: its notes and every action run under it (from the audit log, newest first).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "task id (default: the current task)"},
				"limit": {"type": "integer", "description": "max actions (default 200, max 1000)"}
			}
		}`),
	}
}

func (w WorkGet) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID    string
		Limit int
	}
	_ = json.Unmarshal(payload, &req)
	if req.Limit <= 0 {
		req.Limit = 200
	}
	if req.Limit > 1000 {
		req.Limit = 1000
	}
	var t work.Task
	var ok bool
	if req.ID == "" {
		t, ok = w.Store.Current()
	} else {
		t, ok = w.Store.Get(req.ID)
	}
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "no such task"}
	}
	actions := []security.Entry{}
	if w.AuditPath != "" {
		if got, err := security.ReadAuditFunc(w.AuditPath, req.Limit, func(e security.Entry) bool { return e.Work == t.ID }); err == nil && got != nil {
			actions = got
		}
	}
	return map[string]any{"task": t, "actions": actions}, nil
}

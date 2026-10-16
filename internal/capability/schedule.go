package capability

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/schedule"
)

// RegisterSchedule adds schedule.add/list/delete/run.
func RegisterSchedule(r *Registry, s *schedule.Store) {
	r.Register(ScheduleAdd{Store: s})
	r.Register(ScheduleList{Store: s})
	r.Register(ScheduleDelete{Store: s})
	r.Register(ScheduleRun{Store: s})
}

// --- schedule.add ---

// ScheduleAdd creates or replaces a scheduled call.
type ScheduleAdd struct{ Store *schedule.Store }

func (ScheduleAdd) Name() string { return "schedule.add" }

func (ScheduleAdd) Describe() Descriptor {
	return Descriptor{
		Name: "schedule.add",
		Description: "Schedule a call on this PC, in its local time: at a time of day (optionally only some weekdays) or every N " +
			"minutes/hours. call is any capability or task, e.g. session.lock with {\"hold_until\":\"14:00\"} at 08:00 on " +
			"weekdays, task.backup every 30m, or a recorded macro (task.<name>) at 07:55 once. Runs inside the agent, so it " +
			"keeps working if the panel or the network is down; a run missed while the PC was off fires on return if it is " +
			"at most catch_up_min late (default 60). Same name replaces the entry.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Identifier, [a-zA-Z0-9_-]."},
				"at": {"type": "string", "description": "Local time of day HH:MM (24 h). Exclusive with every."},
				"days": {"type": "array", "items": {"type": "string", "enum": ["mon","tue","wed","thu","fri","sat","sun"]}, "description": "Only these weekdays (with at). Default every day."},
				"every": {"type": "string", "description": "Repeat interval, e.g. 15m, 2h (min 1m). Exclusive with at."},
				"once": {"type": "boolean", "description": "Delete after the first run."},
				"catch_up_min": {"type": "integer", "description": "Max minutes late for a missed at-run to still fire. Default 60, -1 never."},
				"call": {"type": "string", "description": "Capability or task to call, e.g. session.lock, task.backup."},
				"args": {"type": "object", "description": "Arguments for call."},
				"disabled": {"type": "boolean"}
			},
			"required": ["name", "call"]
		}`),
	}
}

func (c ScheduleAdd) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var e schedule.Entry
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, badRequest(err)
	}
	if err := c.Store.Set(&e); err != nil {
		return nil, badRequest(err)
	}
	for _, v := range c.Store.List() {
		if v.Name == e.Name {
			return v, nil
		}
	}
	return e, nil
}

// --- schedule.list ---

// ScheduleList lists the scheduled calls with their next run.
type ScheduleList struct{ Store *schedule.Store }

func (ScheduleList) Name() string { return "schedule.list" }

func (ScheduleList) Describe() Descriptor {
	return Descriptor{
		Name:        "schedule.list",
		Description: "List the calls scheduled on this PC with their next_run, last_run and last_error.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (c ScheduleList) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	return map[string]any{"schedules": c.Store.List()}, nil
}

// --- schedule.delete ---

// ScheduleDelete removes a scheduled call.
type ScheduleDelete struct{ Store *schedule.Store }

func (ScheduleDelete) Name() string { return "schedule.delete" }

func (ScheduleDelete) Describe() Descriptor {
	return Descriptor{
		Name:        "schedule.delete",
		Description: "Delete a scheduled call by name.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`),
	}
}

func (c ScheduleDelete) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if !c.Store.Delete(req.Name) {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf("no such schedule %q", req.Name)}
	}
	return map[string]any{"deleted": req.Name}, nil
}

// --- schedule.run ---

// ScheduleRun fires a scheduled call now, to test it.
type ScheduleRun struct{ Store *schedule.Store }

func (ScheduleRun) Name() string { return "schedule.run" }

func (ScheduleRun) Describe() Descriptor {
	return Descriptor{
		Name:        "schedule.run",
		Description: "Run a scheduled call right now (to test it) and return its result. Counts as its run for today.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`),
	}
}

func (c ScheduleRun) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	return c.Store.RunNow(ctx, req.Name)
}

package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/task"
)

// This is Astrea's "TinyTask": record what a person does with the mouse and
// keyboard, then replay it — on demand, or at a set time with schedule.add.
// A recording is saved as an ordinary macro task (task.<name>), so it reuses
// the macro engine (guard, audit, {{param}} templating) and the scheduler.
//
// Recording needs a global input hook in the desktop session, so the backend
// is Windows-only for now; elsewhere it answers "unavailable".

// RecEvent is one captured input event.
type RecEvent struct {
	T    int64  `json:"t"`           // ms since recording started
	Kind string `json:"kind"`        // move | down | up | key | wheel
	X    int    `json:"x,omitempty"` // move / click position
	Y    int    `json:"y,omitempty"`
	Btn  string `json:"btn,omitempty"`  // left | right | middle
	Keys string `json:"keys,omitempty"` // key.press name, e.g. "ctrl+c"
	Text string `json:"text,omitempty"` // typed characters (coalesced)
	Dy   int    `json:"dy,omitempty"`   // wheel steps
}

// recordBackend captures input in the desktop session.
type recordBackend interface {
	Start() error
	Stop() ([]RecEvent, error)
	Recording() bool
}

var recorder recordBackend

// RecSource is where record.start/stop get their events. In a single process
// it is the local hook backend; on a Windows headless agent the main process
// injects a source that forwards to the GUI helper (which owns the hooks),
// then builds the task itself. StopEvents returns the captured events.
type RecSource interface {
	Start() error
	StopEvents() ([]RecEvent, error)
	Recording() (bool, error)
}

// localSource is the in-process recorder (full agent, or the GUI helper).
type localSource struct{}

func (localSource) Start() error {
	if err := requireRecorder(); err != nil {
		return err
	}
	return recorder.Start()
}

func (localSource) StopEvents() ([]RecEvent, error) {
	if err := requireRecorder(); err != nil {
		return nil, err
	}
	return recorder.Stop()
}

func (localSource) Recording() (bool, error) {
	if recorder == nil {
		return false, nil
	}
	return recorder.Recording(), nil
}

// Local is the in-process recording source.
var Local RecSource = localSource{}

// RecordState tracks an in-progress recording's name and start time; the
// backend holds the events.
type recordState struct {
	mu      sync.Mutex
	name    string
	started time.Time
}

var recState recordState

// Events builds macro steps from captured events. Mouse moves are thinned to
// the ones right before a click (the path in between does not matter), and
// delays between actions become sleep steps so timing roughly matches.
func buildSteps(evs []RecEvent, dropMoves bool) []task.Step {
	var steps []task.Step
	add := func(call string, args map[string]any) {
		b, _ := json.Marshal(args)
		steps = append(steps, task.Step{Call: call, Args: b})
	}
	var last int64
	gap := func(t int64) {
		if d := t - last; d > 400 {
			add("sleep", map[string]any{"ms": d})
		}
		last = t
	}
	for _, e := range evs {
		switch e.Kind {
		case "move":
			// Clicks carry their own position (the down case moves first), so a
			// clean replay drops bare moves. keep_moves records the full path.
			if dropMoves {
				continue
			}
			add("mouse.move", map[string]any{"x": e.X, "y": e.Y})
		case "down":
			gap(e.T)
			add("mouse.move", map[string]any{"x": e.X, "y": e.Y})
			add("mouse.click", map[string]any{"button": orLeft(e.Btn)})
		case "key":
			gap(e.T)
			add("key.press", map[string]any{"keys": e.Keys})
		case "type":
			gap(e.T)
			add("key.type", map[string]any{"text": e.Text})
		case "wheel":
			gap(e.T)
			add("mouse.scroll", map[string]any{"dy": e.Dy})
		}
	}
	return steps
}

func orLeft(b string) string {
	if b == "" {
		return "left"
	}
	return b
}

func requireRecorder() error {
	if recorder == nil {
		return &protocol.Error{Code: protocol.CodeUnavailable, Message: "recording is only available on Windows for now"}
	}
	return nil
}

// RegisterRecord adds record.start / record.stop, backed by the task manager.
// src is where the events come from (Local, or a bridge-backed source).
func RegisterRecord(r *Registry, m *task.Manager, src RecSource) {
	r.Register(RecordStart{Src: src})
	r.Register(RecordStop{Mgr: m, Reg: r, Src: src})
}

// --- record.start ---

// RecordStart begins capturing input.
type RecordStart struct{ Src RecSource }

func (RecordStart) Name() string { return "record.start" }

func (RecordStart) Describe() Descriptor {
	return Descriptor{
		Name: "record.start",
		Description: "Start recording the mouse and keyboard on this PC (like TinyTask). A person then does the task by hand; " +
			"record.stop saves it as a replayable macro. Use it to teach Astrea a repetitive chore once and replay it later, " +
			"on demand or on a schedule. While recording, Astrea's own input is not captured.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (c RecordStart) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	recState.mu.Lock()
	defer recState.mu.Unlock()
	if on, _ := c.Src.Recording(); on {
		return nil, badRequestMsg("already recording; call record.stop first")
	}
	if err := c.Src.Start(); err != nil {
		return nil, internal(err)
	}
	recState.started = time.Now()
	return map[string]any{"recording": true}, nil
}

// --- record.stop ---

// RecordStop ends the recording and saves it as a macro task.
type RecordStop struct {
	Mgr *task.Manager
	Reg *Registry
	Src RecSource
}

func (RecordStop) Name() string { return "record.stop" }

func (RecordStop) Describe() Descriptor {
	return Descriptor{
		Name: "record.stop",
		Description: "Stop recording and save it as a macro task named <name> (callable as task.<name>, schedulable with " +
			"schedule.add). Returns how many steps it captured. keep_moves=true records every mouse move (default: only the " +
			"moves needed to position clicks, so replay is cleaner). discard=true throws the recording away.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string", "description": "Task name to save as, [a-zA-Z0-9_-]."},
				"description": {"type": "string"},
				"keep_moves": {"type": "boolean"},
				"discard": {"type": "boolean"}
			}
		}`),
	}
}

func (c RecordStop) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name      string `json:"name"`
		Desc      string `json:"description"`
		KeepMoves bool   `json:"keep_moves"`
		Discard   bool   `json:"discard"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	recState.mu.Lock()
	defer recState.mu.Unlock()
	if on, _ := c.Src.Recording(); !on {
		return nil, badRequestMsg("not recording")
	}
	evs, err := c.Src.StopEvents()
	if err != nil {
		return nil, internal(err)
	}
	if req.Discard {
		return map[string]any{"discarded": true, "events": len(evs)}, nil
	}
	if req.Name == "" {
		return nil, badRequestMsg("name is required to save the recording (or discard=true)")
	}
	steps := buildSteps(evs, !req.KeepMoves)
	if len(steps) == 0 {
		return nil, badRequestMsg("nothing was recorded")
	}
	desc := req.Desc
	if desc == "" {
		desc = fmt.Sprintf("Recorded macro (%d steps, %s)", len(steps), time.Since(recState.started).Round(time.Second))
	}
	spec := &task.Spec{Name: req.Name, Description: desc, Kind: task.KindMacro, Steps: steps}
	if err := c.Mgr.Define(spec); err != nil {
		return nil, badRequest(err)
	}
	c.Reg.Register(taskTool{mgr: c.Mgr, spec: spec})
	return map[string]any{"name": spec.Name, "tool": "task." + spec.Name, "steps": len(steps)}, nil
}

// --- raw recording for the GUI helper ---
//
// On a Windows headless agent the hooks live in the GUI helper, but the task
// is saved by the main agent. The helper exposes these two raw capabilities;
// the main agent forwards to them (see the bridge source) and builds the task.

// RecordStartRaw starts the local recorder (GUI helper side).
type RecordStartRaw struct{}

func (RecordStartRaw) Name() string { return "record.start" }
func (RecordStartRaw) Describe() Descriptor {
	return Descriptor{Name: "record.start", Description: "Start the input recorder (internal; use record.start on the main agent).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`)}
}
func (RecordStartRaw) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	if err := Local.Start(); err != nil {
		return nil, err
	}
	return map[string]any{"recording": true}, nil
}

// RecordCaptureRaw stops the local recorder and returns the raw events.
type RecordCaptureRaw struct{}

func (RecordCaptureRaw) Name() string { return "record.capture" }
func (RecordCaptureRaw) Describe() Descriptor {
	return Descriptor{Name: "record.capture", Description: "Stop the recorder and return raw events (internal).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`)}
}
func (RecordCaptureRaw) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	evs, err := Local.StopEvents()
	if err != nil {
		return nil, err
	}
	if evs == nil {
		evs = []RecEvent{}
	}
	return map[string]any{"events": evs}, nil
}

// RecordingLocal reports whether the local recorder is running (internal).
type RecordingLocal struct{}

func (RecordingLocal) Name() string { return "record.state" }
func (RecordingLocal) Describe() Descriptor {
	return Descriptor{Name: "record.state", Description: "Whether the recorder is running (internal).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`)}
}
func (RecordingLocal) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	on, _ := Local.Recording()
	return map[string]any{"recording": on}, nil
}

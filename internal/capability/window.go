package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// Window is a top-level window as the Alt+Tab list shows it.
type Window struct {
	Title      string `json:"title"`
	Process    string `json:"process"` // exe name, e.g. zen.exe
	PID        uint32 `json:"pid"`
	State      string `json:"state"` // normal | minimized | maximized
	Foreground bool   `json:"foreground,omitempty"`
	handle     uintptr
}

// windowBackend lists and arranges the desktop's windows. It must run inside
// the user's desktop session (gui helper or full agent).
type windowBackend interface {
	List() ([]Window, error)
	Set(w Window, action string) error
}

var winCtl windowBackend

func requireWindows() error {
	if winCtl == nil {
		return &protocol.Error{Code: protocol.CodeUnavailable, Message: "window control is only implemented on Windows"}
	}
	return nil
}

// WindowList is window.list.
type WindowList struct{}

func (WindowList) Name() string { return "window.list" }

func (WindowList) Describe() Descriptor {
	return Descriptor{
		Name: "window.list",
		Description: "List the open windows on the PC's desktop (what Alt+Tab shows): title, program (exe), state " +
			"(normal/minimized/maximized) and which one is in front. Exact and instant — use it, not a screenshot, to " +
			"know what is open.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (WindowList) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	ws, err := winCtl.List()
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"windows": ws, "count": len(ws)}, nil
}

// WindowSet is window.set.
type WindowSet struct{}

func (WindowSet) Name() string { return "window.set" }

func (WindowSet) Describe() Descriptor {
	return Descriptor{
		Name: "window.set",
		Description: "Bring a window to the front, minimize, maximize, restore or close it — exact, no clicking. Pick the " +
			"window by title (part of it, case-insensitive) and/or process (exe name, e.g. \"zen\" or \"explorer.exe\"). " +
			"If several match, the frontmost one is used; the reply says which and how many matched. close asks the app " +
			"to close, as the X button does (it may still ask to save).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "Part of the window title."},
				"process": {"type": "string", "description": "Program name, with or without .exe."},
				"action": {"type": "string", "enum": ["focus", "minimize", "maximize", "restore", "close"]}
			},
			"required": ["action"]
		}`),
	}
}

func (WindowSet) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	var req struct {
		Title   string `json:"title"`
		Process string `json:"process"`
		Action  string `json:"action"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	switch req.Action {
	case "focus", "minimize", "maximize", "restore", "close":
	default:
		return nil, badRequestMsg("action must be focus, minimize, maximize, restore or close")
	}
	if req.Title == "" && req.Process == "" {
		return nil, badRequestMsg("give title and/or process")
	}
	ws, err := winCtl.List()
	if err != nil {
		return nil, internal(err)
	}
	matches := matchWindows(ws, req.Title, req.Process)
	if len(matches) == 0 {
		titles := make([]string, 0, len(ws))
		for _, w := range ws {
			titles = append(titles, fmt.Sprintf("%q (%s)", w.Title, w.Process))
		}
		return nil, badRequestMsg("no open window matches; open windows: " + strings.Join(titles, ", "))
	}
	power.MarkInjected() // our doing, not a person at the PC
	w := matches[0]
	if err := winCtl.Set(w, req.Action); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"window": w.Title, "process": w.Process, "action": req.Action, "matched": len(matches)}, nil
}

// matchWindows keeps the windows whose title contains title and whose exe is
// process (".exe" optional), in the list's order (frontmost first).
func matchWindows(ws []Window, title, process string) []Window {
	title = strings.ToLower(title)
	process = strings.TrimSuffix(strings.ToLower(process), ".exe")
	var out []Window
	for _, w := range ws {
		if title != "" && !strings.Contains(strings.ToLower(w.Title), title) {
			continue
		}
		if process != "" && strings.TrimSuffix(strings.ToLower(w.Process), ".exe") != process {
			continue
		}
		out = append(out, w)
	}
	return out
}

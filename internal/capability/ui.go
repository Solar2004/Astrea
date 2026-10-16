package capability

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// Fine-grained desktop control ("manos finas"), layer 2: read the Windows UI
// Automation tree of the foreground window as a list of interactive elements
// (button/field/menu with their name, role and position) and act on them by
// id. No model, no GPU: instant, exact text, covers most native apps. When the
// tree has nothing useful (games, canvases, apps drawn by hand), fall back to
// vision.locate (layer 3).
//
// Backend is Windows-only (UI Automation); elsewhere it answers "unavailable".

// UIElement is one interactive control found on screen.
type UIElement struct {
	ID      int    `json:"id"`
	Role    string `json:"role"`            // button, edit, menuitem, checkbox…
	Name    string `json:"name"`            // its label/text
	Value   string `json:"value,omitempty"` // current value of an edit/combo
	X       int    `json:"x"`               // center, screen coordinates
	Y       int    `json:"y"`
	W       int    `json:"w,omitempty"`
	H       int    `json:"h,omitempty"`
	Enabled bool   `json:"enabled"`
}

// uiBackend reads the accessibility tree and acts on it.
type uiBackend interface {
	// Snapshot lists interactive elements of the foreground window (or, with
	// window!="", the first top-level window whose title contains it).
	Snapshot(window string) (title string, els []UIElement, err error)
	// Center returns the screen center of the element last snapshotted with id.
	Center(id int) (x, y int, ok bool)
	// Last returns the elements of the most recent snapshot.
	Last() []UIElement
}

var ui uiBackend

func requireUI() error {
	if ui == nil {
		return &protocol.Error{Code: protocol.CodeUnavailable, Message: "ui.* (UI Automation) is only available on Windows"}
	}
	return nil
}

// RegisterUI adds ui.snapshot / ui.find / ui.act.
func RegisterUI(r *Registry) {
	r.Register(UISnapshot{})
	r.Register(UIFind{})
	r.Register(UIAct{})
}

// --- ui.snapshot ---

// UISnapshot lists the interactive elements of the foreground window.
type UISnapshot struct{}

func (UISnapshot) Name() string { return "ui.snapshot" }

func (UISnapshot) Describe() Descriptor {
	return Descriptor{
		Name: "ui.snapshot",
		Description: "Read the controlled PC's foreground window as a numbered list of interactive elements (buttons, fields, " +
			"menus) with their name, role and position — not a screenshot, so far fewer tokens and exact labels. Then act " +
			"with ui.act by id. Works on native Windows apps (Win32/WinUI/Office). If the list is empty or misses what you " +
			"need (a game, a canvas), fall back to a screenshot or vision.locate. window filters to a top-level window whose " +
			"title contains that text.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"window": {"type": "string", "description": "Only this top-level window (substring of its title). Default: the foreground one."}}}`),
	}
}

func (UISnapshot) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireUI(); err != nil {
		return nil, err
	}
	var req struct {
		Window string `json:"window"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	title, els, err := ui.Snapshot(req.Window)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"window": title, "count": len(els), "elements": els}, nil
}

// --- ui.find ---

// UIFind returns the elements whose name matches a query.
type UIFind struct{}

func (UIFind) Name() string { return "ui.find" }

func (UIFind) Describe() Descriptor {
	return Descriptor{
		Name: "ui.find",
		Description: "Find interactive elements of the foreground window whose name contains text (case-insensitive). A quick " +
			"way to locate one control without reading the whole snapshot. Returns the same ids as ui.snapshot.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"text": {"type": "string"}, "window": {"type": "string"}}, "required": ["text"]}`),
	}
}

func (UIFind) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireUI(); err != nil {
		return nil, err
	}
	var req struct {
		Text   string `json:"text"`
		Window string `json:"window"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Text == "" {
		return nil, badRequestMsg("text is required")
	}
	title, els, err := ui.Snapshot(req.Window)
	if err != nil {
		return nil, internal(err)
	}
	q := strings.ToLower(req.Text)
	var out []UIElement
	for _, e := range els {
		if strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	return map[string]any{"window": title, "matches": out}, nil
}

// --- ui.act ---

// UIAct clicks or fills an element by id (from the last snapshot) or by name.
type UIAct struct{}

func (UIAct) Name() string { return "ui.act" }

func (UIAct) Describe() Descriptor {
	return Descriptor{
		Name: "ui.act",
		Description: "Click or fill an element found by ui.snapshot/ui.find. Identify it with id (from the latest snapshot) or " +
			"name (exact label; re-scans if needed). action=click (default) moves the mouse to its center and clicks; " +
			"action=fill clicks it and types value. Astrea signs the PC in first if it is locked.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "integer", "description": "Element id from the latest ui.snapshot/ui.find."},
				"name": {"type": "string", "description": "Element label, if you do not have an id."},
				"action": {"type": "string", "enum": ["click", "fill"], "description": "Default click."},
				"value": {"type": "string", "description": "Text to type when action=fill."},
				"double": {"type": "boolean", "description": "Double-click."},
				"button": {"type": "string", "enum": ["left", "right", "middle"]}
			}
		}`),
	}
}

func (UIAct) Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error) {
	if err := requireUI(); err != nil {
		return nil, err
	}
	if err := requireInput(); err != nil {
		return nil, err
	}
	var req struct {
		ID     *int   `json:"id"`
		Name   string `json:"name"`
		Action string `json:"action"`
		Value  string `json:"value"`
		Double bool   `json:"double"`
		Button string `json:"button"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	var x, y int
	var matched string
	if req.ID != nil {
		cx, cy, ok := ui.Center(*req.ID)
		if !ok {
			return nil, badRequestMsg("no element with that id in the latest snapshot; call ui.snapshot again")
		}
		x, y = cx, cy
	} else if req.Name != "" {
		// Look in the latest snapshot first (the window the AI just read), then
		// fall back to a fresh scan of the foreground. This keeps the common
		// snapshot→act flow working even if the focus moved in between.
		find := func(els []UIElement) bool {
			q := strings.ToLower(req.Name)
			for _, e := range els {
				if e.Name == req.Name {
					x, y, matched = e.X, e.Y, e.Name
					return true
				}
			}
			for _, e := range els {
				if strings.Contains(strings.ToLower(e.Name), q) {
					x, y, matched = e.X, e.Y, e.Name
					return true
				}
			}
			return false
		}
		found := find(ui.Last())
		if !found {
			_, els, err := ui.Snapshot("")
			if err != nil {
				return nil, internal(err)
			}
			found = find(els)
		}
		if !found {
			return nil, badRequestMsg("no element named like that in the latest snapshot or the foreground window")
		}
	} else {
		return nil, badRequestMsg("give an id or a name")
	}

	button := req.Button
	if button == "" {
		button = "left"
	}
	power.MarkInjected()
	defer power.MarkInjected()
	if err := input.MouseMove(x, y); err != nil {
		return nil, internal(err)
	}
	if err := input.MouseClick(button, req.Double); err != nil {
		return nil, internal(err)
	}
	if req.Action == "fill" {
		if err := input.KeyType(req.Value); err != nil {
			return nil, internal(err)
		}
	}
	res := map[string]any{"ok": true, "x": x, "y": y, "action": orClick(req.Action)}
	if matched != "" {
		res["matched"] = matched
	}
	return res, nil
}

func orClick(a string) string {
	if a == "" {
		return "click"
	}
	return a
}

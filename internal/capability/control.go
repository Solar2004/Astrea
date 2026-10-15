package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// Human control: while the owner drives this PC from the panel, the AI's
// mouse and keyboard calls are refused. The panel marks its own calls with
// "from": "panel" and keeps the mode alive with panel.control heartbeats; it
// expires on its own if the panel goes away.
var human struct {
	sync.Mutex
	until time.Time
}

func humanActive() bool {
	human.Lock()
	defer human.Unlock()
	return time.Now().Before(human.until)
}

// errHuman is what the AI gets while a person has the controls.
var errHuman = &protocol.Error{Code: protocol.CodeDenied, Message: "a person has taken control of this PC's mouse and keyboard from the Astrea panel. " +
	"Do not fight for it: wait until they release it (the screen is still yours to look at with screen.capture) or ask the owner."}

// checkInput gates every input capability: backend present and, under human
// control, only the panel's own calls.
func checkInput(payload json.RawMessage) error {
	if err := requireInput(); err != nil {
		return err
	}
	if !humanActive() {
		return nil
	}
	var from struct {
		From string `json:"from"`
	}
	_ = json.Unmarshal(payload, &from)
	if from.From != "panel" {
		return errHuman
	}
	return nil
}

// PanelControl turns human control on or off. Called by the panel, not the AI.
type PanelControl struct{}

func (PanelControl) Name() string { return "panel.control" }

func (PanelControl) Describe() Descriptor {
	return Descriptor{
		Name:        "panel.control",
		Description: "Panel only: a person takes (on) or releases (off) the mouse and keyboard; while on, the AI's input calls are refused.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"on": {"type": "boolean"}, "ttl_sec": {"type": "integer"}}, "required": ["on"]}`),
	}
}

func (PanelControl) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		On     bool `json:"on"`
		TTLSec int  `json:"ttl_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	ttl := time.Duration(min(max(req.TTLSec, 5), 300)) * time.Second
	human.Lock()
	defer human.Unlock()
	if req.On {
		human.until = time.Now().Add(ttl)
	} else {
		human.until = time.Time{}
	}
	return map[string]any{"human_control": req.On}, nil
}

// --- mouse.button ---

// MouseButton presses or releases a button, for drags.
type MouseButton struct{}

func (MouseButton) Name() string { return "mouse.button" }

func (MouseButton) Describe() Descriptor {
	return Descriptor{
		Name:        "mouse.button",
		Description: "Press (down) or release (up) a mouse button at the current position, e.g. to drag: mouse.move, down, mouse.move, up.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"button": {"type": "string", "enum": ["left", "right", "middle"], "description": "Default left."},
				"action": {"type": "string", "enum": ["down", "up"]}
			},
			"required": ["action"]
		}`),
	}
}

func (MouseButton) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	var req struct{ Button, Action string }
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Action != "down" && req.Action != "up" {
		return nil, badRequest(fmt.Errorf("action must be down or up"))
	}
	if req.Button == "" {
		req.Button = "left"
	}
	if err := input.MouseButton(req.Button, req.Action == "down"); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"button": req.Button, "action": req.Action}, nil
}

// --- mouse.scroll ---

// MouseScroll turns the wheel at the current position.
type MouseScroll struct{}

func (MouseScroll) Name() string { return "mouse.scroll" }

func (MouseScroll) Describe() Descriptor {
	return Descriptor{
		Name:        "mouse.scroll",
		Description: "Scroll the mouse wheel at the current cursor position, in notches: dy > 0 scrolls down, dy < 0 up; dx > 0 right.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"dy": {"type": "integer"}, "dx": {"type": "integer"}}}`),
	}
}

func (MouseScroll) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	var req struct{ DX, DY int }
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	req.DX, req.DY = min(max(req.DX, -50), 50), min(max(req.DY, -50), 50)
	if err := input.Scroll(req.DX, req.DY); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"dx": req.DX, "dy": req.DY}, nil
}

// --- key.press ---

// keyNames are the keys key.press understands, besides single letters and digits.
var keyNames = []string{"ctrl", "shift", "alt", "win", "enter", "esc", "tab", "backspace", "space",
	"delete", "insert", "home", "end", "pageup", "pagedown", "left", "up", "right", "down",
	"capslock", "printscreen", "menu",
	"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12"}

var keyAliases = map[string]string{"control": "ctrl", "cmd": "win", "super": "win", "meta": "win",
	"windows": "win", "return": "enter", "escape": "esc", "del": "delete", "ins": "insert",
	"pgup": "pageup", "pgdn": "pagedown", "arrowleft": "left", "arrowup": "up",
	"arrowright": "right", "arrowdown": "down", "spacebar": " ", "option": "alt"}

// parseKeys turns "Ctrl+Shift+Esc" into ["ctrl" "shift" "esc"].
func parseKeys(combo string) ([]string, error) {
	combo = strings.TrimSpace(strings.ToLower(combo))
	if combo == "+" {
		return nil, fmt.Errorf("use key.type for symbols")
	}
	var out []string
	for _, k := range strings.Split(combo, "+") {
		k = strings.TrimSpace(k)
		if a, ok := keyAliases[k]; ok {
			k = a
		}
		if k == " " {
			k = "space"
		}
		valid := len(k) == 1 && (k[0] >= 'a' && k[0] <= 'z' || k[0] >= '0' && k[0] <= '9')
		for _, n := range keyNames {
			valid = valid || k == n
		}
		if !valid {
			return nil, fmt.Errorf("unknown key %q (known: letters, digits, %s; use key.type for text and symbols)", k, strings.Join(keyNames, ", "))
		}
		out = append(out, k)
	}
	return out, nil
}

// KeyPress presses a key or a combination.
type KeyPress struct{}

func (KeyPress) Name() string { return "key.press" }

func (KeyPress) Describe() Descriptor {
	return Descriptor{
		Name: "key.press",
		Description: "Press a key or a key combination, e.g. \"enter\", \"ctrl+c\", \"alt+tab\", \"win+r\", \"ctrl+shift+esc\", \"f5\". " +
			"Modifiers are held while the last key is pressed. For typing text use key.type.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"keys": {"type": "string"}}, "required": ["keys"]}`),
	}
}

func (KeyPress) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	var req struct {
		Keys string `json:"keys"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	keys, err := parseKeys(req.Keys)
	if err != nil {
		return nil, badRequest(err)
	}
	if err := input.KeyPress(keys); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"pressed": strings.Join(keys, "+")}, nil
}

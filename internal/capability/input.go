package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// inputBackend is the platform-specific implementation of desktop input.
// Exactly one is assigned to `input` at init time per build (see input_*.go).
type inputBackend interface {
	MouseMove(x, y int) error
	MouseClick(button string, double bool) error
	KeyType(text string) error
	MouseButton(button string, down bool) error
	Scroll(dx, dy int) error
	KeyPress(keys []string) error // modifiers first, as parsed by parseKeys
}

// input is the active backend for the build's OS.
var input inputBackend

func requireInput() error {
	if input == nil {
		return &protocol.Error{Code: protocol.CodeDenied, Message: "desktop input not supported on this OS build"}
	}
	return nil
}

// --- mouse.move ---

// MouseMove moves the cursor to absolute screen coordinates.
type MouseMove struct{}

func (MouseMove) Name() string { return "mouse.move" }

func (MouseMove) Describe() Descriptor {
	return Descriptor{
		Name:        "mouse.move",
		Description: "Move the mouse cursor to absolute screen coordinates on the controlled machine.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"x": {"type": "integer"}, "y": {"type": "integer"}},
			"required": ["x", "y"]
		}`),
	}
}

func (MouseMove) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	var req struct{ X, Y int }
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	power.MarkInjected() // our input is not a person at the PC
	defer power.MarkInjected()
	if err := input.MouseMove(req.X, req.Y); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"x": req.X, "y": req.Y}, nil
}

// --- mouse.click ---

// MouseClick presses a mouse button at the current cursor position.
type MouseClick struct{}

func (MouseClick) Name() string { return "mouse.click" }

func (MouseClick) Describe() Descriptor {
	return Descriptor{
		Name:        "mouse.click",
		Description: "Click a mouse button at the current cursor position. Move first with mouse.move if needed.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"button": {"type": "string", "enum": ["left", "right", "middle"], "description": "Default left."},
				"double": {"type": "boolean", "description": "Double-click."}
			}
		}`),
	}
}

func (MouseClick) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	req := struct {
		Button string `json:"button"`
		Double bool   `json:"double"`
	}{Button: "left"}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}
	if req.Button == "" {
		req.Button = "left"
	}
	power.MarkInjected() // our input is not a person at the PC
	defer power.MarkInjected()
	if err := input.MouseClick(req.Button, req.Double); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"button": req.Button, "double": req.Double}, nil
}

// --- key.type ---

// KeyType types a string of text as keyboard input.
type KeyType struct{}

func (KeyType) Name() string { return "key.type" }

func (KeyType) Describe() Descriptor {
	return Descriptor{
		Name:        "key.type",
		Description: "Type Unicode text as keyboard input on the controlled machine.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"text": {"type": "string"}},
			"required": ["text"]
		}`),
	}
}

func (KeyType) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := checkInput(payload); err != nil {
		return nil, err
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	power.MarkInjected() // our input is not a person at the PC
	defer power.MarkInjected()
	if err := input.KeyType(req.Text); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"typed": len(req.Text)}, nil
}

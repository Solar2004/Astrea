package capability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/kbinani/screenshot"
)

// ScreenCapture grabs a screenshot of a display on the controlled machine and
// returns it as a base64-encoded PNG.
type ScreenCapture struct{}

func (ScreenCapture) Name() string { return "screen.capture" }

func (ScreenCapture) Describe() Descriptor {
	return Descriptor{
		Name:        "screen.capture",
		Description: "Capture a screenshot of the controlled machine. Returns a base64-encoded PNG. Use display -1 (default) to capture all displays combined.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"display": {"type": "integer", "description": "Display index (0-based). -1 = all displays combined. Default -1."}
			}
		}`),
	}
}

func (ScreenCapture) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	req := struct {
		Display int `json:"display"`
	}{Display: -1}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}

	n := screenshot.NumActiveDisplays()
	if n == 0 {
		return nil, &protocol.Error{Code: protocol.CodeInternal, Message: "no active displays (is the agent running in a GUI session?)"}
	}

	bounds := screenshot.GetDisplayBounds(0)
	if req.Display >= 0 {
		if req.Display >= n {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "display index out of range"}
		}
		bounds = screenshot.GetDisplayBounds(req.Display)
	} else {
		// Combined virtual bounds across all displays.
		for i := 1; i < n; i++ {
			bounds = bounds.Union(screenshot.GetDisplayBounds(i))
		}
	}

	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, internal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, internal(err)
	}
	return map[string]any{
		"width":  bounds.Dx(),
		"height": bounds.Dy(),
		"format": "png",
		"base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

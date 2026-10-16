package capability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/kbinani/screenshot"
)

// Image is an image attached to a capability result. The hub turns every entry
// of a result's top-level "images" array into native MCP image content, so the
// model sees the picture instead of a base64 string.
type Image struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"` // base64
}

// ScreenCapture grabs a screenshot of a display on the controlled machine and
// returns it as an image, downscaled so it fits a model's vision input.
type ScreenCapture struct{}

func (ScreenCapture) Name() string { return "screen.capture" }

func (ScreenCapture) Describe() Descriptor {
	return Descriptor{
		Name: "screen.capture",
		Description: "Capture a screenshot of the controlled machine and return it as an image. " +
			"The image may be downscaled: to click something seen at image pixel (ix, iy), " +
			"use screen coordinates x = origin.x + ix/scale, y = origin.y + iy/scale.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"display": {"type": "integer", "description": "Display index (0-based). -1 = all displays combined. Default -1."},
				"max_width": {"type": "integer", "description": "Downscale to at most this width in pixels. Default 1568; 0 = full resolution."},
				"format": {"type": "string", "enum": ["jpeg", "png"], "description": "Default jpeg (much smaller)."},
				"quality": {"type": "integer", "description": "JPEG quality 1-100. Default 80."},
				"peek": {"type": "boolean", "description": "Only look: if the PC is on the lock screen, do not sign in first (default signs in)."}
			}
		}`),
	}
}

func (ScreenCapture) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	req := struct {
		Display  int    `json:"display"`
		MaxWidth *int   `json:"max_width"`
		Format   string `json:"format"`
		Quality  int    `json:"quality"`
	}{Display: -1}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}
	maxWidth := 1568
	if req.MaxWidth != nil {
		maxWidth = *req.MaxWidth
	}
	if req.Quality <= 0 || req.Quality > 100 {
		req.Quality = 80
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

	shot, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return nil, internal(err)
	}
	var img image.Image = shot
	scale := 1.0
	if maxWidth > 0 && bounds.Dx() > maxWidth {
		scale = float64(maxWidth) / float64(bounds.Dx())
		img = downscale(shot, maxWidth, int(float64(bounds.Dy())*scale+0.5))
	}

	var buf bytes.Buffer
	mime := "image/jpeg"
	if req.Format == "png" {
		mime = "image/png"
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: req.Quality})
	}
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{
		"screen_width":  bounds.Dx(),
		"screen_height": bounds.Dy(),
		"image_width":   img.Bounds().Dx(),
		"image_height":  img.Bounds().Dy(),
		"origin":        map[string]int{"x": bounds.Min.X, "y": bounds.Min.Y},
		"scale":         scale,
		"images":        []Image{{MimeType: mime, Data: base64.StdEncoding.EncodeToString(buf.Bytes())}},
	}, nil
}

// downscale resizes src to w×h by averaging the source pixels that fall in
// each destination pixel (box filter): sharp enough to read UI text.
func downscale(src *image.RGBA, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, (y+1)*sh/h
		if y1 == y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, (x+1)*sw/w
			if x1 == x0 {
				x1 = x0 + 1
			}
			var r, g, b, n uint32
			for sy := y0; sy < y1; sy++ {
				off := src.PixOffset(sb.Min.X+x0, sb.Min.Y+sy)
				for sx := x0; sx < x1; sx++ {
					r += uint32(src.Pix[off])
					g += uint32(src.Pix[off+1])
					b += uint32(src.Pix[off+2])
					off += 4
					n++
				}
			}
			d := dst.PixOffset(x, y)
			dst.Pix[d], dst.Pix[d+1], dst.Pix[d+2], dst.Pix[d+3] = uint8(r/n), uint8(g/n), uint8(b/n), 255
		}
	}
	return dst
}

package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// Fine-grained desktop control ("manos finas"), layer 3: a local vision model
// (e.g. UI-TARS-1.5-7B served on the PC's own GPU) that turns "click the submit
// button" plus a screenshot into screen coordinates. This is the fallback for
// when the browser (layer 1) and the UI Automation tree (layer 2) cannot see
// the target — games, canvases, apps drawn by hand. The model only *sees*;
// Hermes still decides.
//
// The model is just another supervised service in Astrea: download it with
// net.download (hf://), run it with service.start behind an OpenAI-compatible
// server (vLLM), and point vision.locate at it. Nothing here needs a GPU; the
// service does.

// VisionConfig points vision.locate at the local model server and says how to
// start it on demand.
type VisionConfig struct {
	// Endpoint is an OpenAI-compatible chat/completions URL, e.g.
	// http://127.0.0.1:8100/v1/chat/completions.
	Endpoint string `json:"endpoint"`
	// Model is the served model name.
	Model string `json:"model"`
	// APIKey is sent as a bearer token if the server wants one.
	APIKey string `json:"api_key,omitempty"`
	// Serve is the command (argv) that launches the model server, e.g.
	// ["python","-m","vllm.entrypoints.openai.api_server","--model",
	// "ByteDance-Seed/UI-TARS-1.5-7B","--port","8100"]. Empty: the model is
	// managed outside Astrea (vision.start/stop only reuse a running server).
	Serve []string `json:"serve,omitempty"`
	// IdleStopSec auto-stops the model after this many seconds with no use,
	// freeing the GPU. Default 300 (5 min); 0 uses the default.
	IdleStopSec int `json:"idle_stop_sec,omitempty"`
	// Coords is how the model reports coordinates: "" / "norm1000" (UI-TARS
	// default, 0-1000 of the screen) or "pixel" (image pixels, some Qwen2.5-VL).
	Coords string `json:"coords,omitempty"`
}

// Dispatcher runs another capability (used to grab a screenshot and to click).
type Dispatcher func(ctx context.Context, method string, args json.RawMessage) (any, error)

// errNoVision is returned when no model endpoint is configured.
var errNoVision = &protocol.Error{Code: protocol.CodeUnavailable, Message: "vision model not configured: set vision.endpoint/model (and vision.serve to start it on demand) in config.json — see docs/VISION.md"}

// VisionLocate finds a target on screen with the local vision model.
type VisionLocate struct {
	Cfg      VisionConfig // used only without Mgr
	Dispatch Dispatcher
	Mgr      *VisionManager
}

// cfg is the live config: auto-provisioning fills in the endpoint after the
// capability is registered, so a copy taken at registration stays empty.
func (c VisionLocate) cfg() VisionConfig {
	if c.Mgr != nil {
		return c.Mgr.Config()
	}
	return c.Cfg
}

// RegisterVision adds vision.locate/start/stop/status/setup, all sharing one
// manager (and provisioner, which may be nil if auto-provisioning is unwired).
func RegisterVision(r *Registry, mgr *VisionManager, prov *Provisioner, dispatch Dispatcher) {
	r.Register(VisionLocate{Dispatch: dispatch, Mgr: mgr})
	r.Register(VisionStart{Mgr: mgr})
	r.Register(VisionStop{Mgr: mgr})
	r.Register(VisionStatus{Mgr: mgr, Prov: prov})
	if prov != nil {
		r.Register(VisionSetup{Prov: prov})
	}
}

func (VisionLocate) Name() string { return "vision.locate" }

func (VisionLocate) Describe() Descriptor {
	return Descriptor{
		Name: "vision.locate",
		Description: "Find something on screen by description, using the local vision model on this PC's GPU, and return its " +
			"screen coordinates (x, y). Use it only when ui.snapshot (native apps) and browser.read (web) cannot see the " +
			"target — a game, a canvas, a custom-drawn app. instruction is what to find, e.g. \"the red Submit button\". " +
			"With click=true it also clicks there. The model is started on demand (and auto-stops when idle to free the " +
			"GPU); the first call after a cold start waits for it to load, so it can take a minute.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"instruction": {"type": "string", "description": "What to locate, in plain language."},
				"click": {"type": "boolean", "description": "Also click the located point."},
				"double": {"type": "boolean", "description": "Double-click (with click=true)."}
			},
			"required": ["instruction"]
		}`),
	}
}

func (c VisionLocate) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if !c.Mgr.Configured() {
		return nil, errNoVision
	}
	var req struct {
		Instruction string `json:"instruction"`
		Click       bool   `json:"click"`
		Double      bool   `json:"double"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return nil, badRequestMsg("instruction is required")
	}
	// Bring the model up on demand (reused if already running), then mark use
	// so the idle auto-stop timer resets.
	if err := c.Mgr.Start(ctx, 0); err != nil {
		return nil, internal(err)
	}
	c.Mgr.touch()

	// 1. Grab a screenshot through the normal capability (forwarded to the GUI
	// helper on a headless agent, and auto-unlocked if the PC is locked).
	shotRaw, err := c.Dispatch(ctx, "screen.capture", json.RawMessage(`{"max_width":1568,"format":"png"}`))
	if err != nil {
		return nil, internal(fmt.Errorf("screenshot: %w", err))
	}
	shot, err := asShot(shotRaw)
	if err != nil {
		return nil, internal(err)
	}
	if len(shot.Images) == 0 {
		return nil, internal(fmt.Errorf("no screenshot returned"))
	}

	// 2. Ask the model where the target is. It answers in its own coordinate
	// space (UI-TARS: normalized 0-1000; some models: pixels of the image).
	mx, my, err := c.query(ctx, req.Instruction, shot.Images[0].MimeType, shot.Images[0].Data)
	if err != nil {
		return nil, internal(err)
	}

	// 3. Model coordinates → screen coordinates.
	scale := shot.Scale
	if scale <= 0 {
		scale = 1
	}
	var x, y int
	if c.coordsPixel() {
		// Pixels of the image as the model saw it (resized to multiples of
		// 28), back to our image, then screen = origin + px/scale.
		iw := float64(shot.ScreenWidth) * scale
		ih := float64(shot.ScreenHeight) * scale
		mx, my = mx*iw/grid28(iw), my*ih/grid28(ih)
		x = shot.Origin.X + int(math.Round(mx/scale))
		y = shot.Origin.Y + int(math.Round(my/scale))
	} else {
		// UI-TARS default: normalized 0-1000 of the screen region.
		w, h := float64(shot.ScreenWidth), float64(shot.ScreenHeight)
		x = shot.Origin.X + int(math.Round(mx/1000*w))
		y = shot.Origin.Y + int(math.Round(my/1000*h))
	}
	res := map[string]any{"x": x, "y": y, "model_x": mx, "model_y": my}

	if req.Click {
		args, _ := json.Marshal(map[string]any{"x": x, "y": y})
		if _, err := c.Dispatch(ctx, "mouse.move", args); err != nil {
			return nil, internal(fmt.Errorf("move: %w", err))
		}
		clickArgs, _ := json.Marshal(map[string]any{"double": req.Double})
		if _, err := c.Dispatch(ctx, "mouse.click", clickArgs); err != nil {
			return nil, internal(fmt.Errorf("click: %w", err))
		}
		res["clicked"] = true
	}
	return res, nil
}

// grid28 is a side as Qwen2.5-VL (UI-TARS 1.5) resizes it: to the nearest
// multiple of 28 (14 px patches merged 2×2). Answers in "pixel" mode are in
// that resized image, e.g. 896 rows for an 882-row screenshot.
func grid28(side float64) float64 {
	return max(28, math.Round(side/28)*28)
}

type shotResult struct {
	Scale        float64 `json:"scale"`
	ScreenWidth  int     `json:"screen_width"`
	ScreenHeight int     `json:"screen_height"`
	Origin       struct {
		X int `json:"x"`
		Y int `json:"y"`
	} `json:"origin"`
	Images []Image `json:"images"`
}

// coordsPixel reports whether the model answers in image pixels (some
// Qwen2.5-VL checkpoints) rather than UI-TARS normalized 0-1000 (the default).
func (c VisionLocate) coordsPixel() bool {
	return strings.EqualFold(c.cfg().Coords, "pixel")
}

func asShot(v any) (shotResult, error) {
	var s shotResult
	b, err := json.Marshal(v)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// coordRe pulls the first two numbers out of the model's answer, so it works
// whether the model replies with a UI-TARS action — click(point='<point>512
// 384</point>') or click(start_box='(512,384)') — or a bare "512, 384".
var coordRe = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

// groundingPrompt is UI-TARS's own action-only template (codes/ui_tars/prompt.py):
// the model replies with just "Action: click(point='<point>x y</point>')".
const groundingPrompt = "You are a GUI agent. You are given a task and your action history, with screenshots. " +
	"You need to perform the next action to complete the task.\n\n## Output Format\nAction: ...\n\n" +
	"## Action Space\nclick(point='<point>x1 y1</point>')\n\n## User Instruction\nLocate: %s"

func (c VisionLocate) query(ctx context.Context, instruction, mime, b64 string) (float64, float64, error) {
	cfg := c.cfg()
	prompt := fmt.Sprintf(groundingPrompt, instruction)

	body := map[string]any{
		"model":       cfg.Model,
		"temperature": 0,
		"max_tokens":  64,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": prompt},
				{"type": "image_url", "image_url": map[string]string{
					"url": "data:" + mime + ";base64," + b64,
				}},
			},
		}},
	}
	buf, _ := json.Marshal(body)
	// 16 GB GPUs answer in seconds; a 4 GB GPU running part of the model on
	// the CPU takes minutes (a GTX 1650 laptop did not finish in 3).
	hctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(hctx, "POST", cfg.Endpoint, bytes.NewReader(buf))
	if err != nil {
		return 0, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("vision model unreachable: %w (is the service running?)", err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, fmt.Errorf("decode model reply: %w", err)
	}
	if len(out.Choices) == 0 {
		return 0, 0, fmt.Errorf("model returned no answer: %s", string(out.Error))
	}
	nums := coordRe.FindAllString(out.Choices[0].Message.Content, -1)
	if len(nums) < 2 {
		return 0, 0, fmt.Errorf("model did not return coordinates: %q", out.Choices[0].Message.Content)
	}
	var fx, fy float64
	fmt.Sscanf(nums[0], "%g", &fx)
	fmt.Sscanf(nums[1], "%g", &fy)
	return fx, fy, nil
}

// --- vision.start ---

// VisionStart brings the model up (on demand) and waits for it to load.
type VisionStart struct{ Mgr *VisionManager }

func (VisionStart) Name() string { return "vision.start" }

func (VisionStart) Describe() Descriptor {
	return Descriptor{
		Name: "vision.start",
		Description: "Start the local vision model on this PC's GPU and wait until it is ready. It is off by default; call this " +
			"before a batch of vision.locate work (or just call vision.locate, which starts it on demand). It auto-stops when " +
			"idle. First start loads the weights into the GPU and can take ~30-90s. wait_sec caps the wait (default 180).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"wait_sec": {"type": "integer"}}}`),
	}
}

func (c VisionStart) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		WaitSec int `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if err := c.Mgr.Start(ctx, req.WaitSec); err != nil {
		return nil, internal(err)
	}
	return c.Mgr.Status(ctx), nil
}

// --- vision.stop ---

// VisionStop stops the model, freeing the GPU.
type VisionStop struct{ Mgr *VisionManager }

func (VisionStop) Name() string { return "vision.stop" }

func (VisionStop) Describe() Descriptor {
	return Descriptor{
		Name:        "vision.stop",
		Description: "Stop the local vision model now, freeing its GPU memory. It also stops by itself after being idle.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (c VisionStop) Invoke(_ context.Context, _ json.RawMessage, _ Emit) (any, error) {
	return map[string]any{"stopped": c.Mgr.Stop()}, nil
}

// --- vision.status ---

// VisionStatus reports whether the model runs and its resource use.
type VisionStatus struct {
	Mgr  *VisionManager
	Prov *Provisioner
}

func (VisionStatus) Name() string { return "vision.status" }

func (VisionStatus) Describe() Descriptor {
	return Descriptor{
		Name: "vision.status",
		Description: "Report the vision model: whether it is configured, running and ready, its uptime, idle time, process RAM " +
			"and the GPU memory in use (used/total). Use it to check what the model is consuming before or after locate work.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (c VisionStatus) Invoke(ctx context.Context, _ json.RawMessage, _ Emit) (any, error) {
	st := c.Mgr.Status(ctx)
	if c.Prov != nil {
		st["provision"] = c.Prov.Status()
	}
	return st, nil
}

// --- vision.setup ---

// VisionSetup turns auto-provisioning (download + wire-up) on or off.
type VisionSetup struct{ Prov *Provisioner }

func (VisionSetup) Name() string { return "vision.setup" }

func (VisionSetup) Describe() Descriptor {
	return Descriptor{
		Name: "vision.setup",
		Description: "Turn on (or off) automatic setup of the local vision model on this PC. It is ON by default on every " +
			"Windows PC with an NVIDIA GPU (the model is required); on=false opts this PC out. With on=true, the agent downloads " +
			"the model (and, on Windows, the llama.cpp CUDA runtime) by itself as soon as it has internet — resumably, in the " +
			"background, retrying until done — then wires vision.serve automatically so vision.locate just works. The switch is " +
			"remembered across restarts. Without arguments, reports the setup progress (download MB, phase, ready).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"on": {"type": "boolean", "description": "true: start/keep auto-setup. false: stop it. Omit: report progress."}}}`),
	}
}

func (c VisionSetup) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		On *bool `json:"on"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.On != nil {
		if err := c.Prov.SetOn(*req.On); err != nil {
			return nil, internal(err)
		}
	}
	return c.Prov.Status(), nil
}

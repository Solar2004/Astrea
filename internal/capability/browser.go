package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/browser"
)

// RegisterBrowser adds the browser.* capabilities, all backed by one headful
// browser Astrea launches with its own profile.
func RegisterBrowser(r *Registry, b *browser.Browser) {
	r.Register(BrowserOpen{B: b})
	r.Register(BrowserRead{B: b})
	r.Register(BrowserAct{B: b})
	r.Register(BrowserEval{B: b})
	r.Register(BrowserShot{B: b})
}

// --- browser.open ---

// BrowserOpen navigates the browser to a URL.
type BrowserOpen struct{ B *browser.Browser }

func (BrowserOpen) Name() string { return "browser.open" }

func (BrowserOpen) Describe() Descriptor {
	return Descriptor{
		Name: "browser.open",
		Description: "Open a URL in a real browser Astrea runs on the controlled PC (its own window and profile, separate from " +
			"the person's). Launches the browser on first use. Returns the final url and title. Without url, reports the " +
			"current page. Use browser.read to get the page as text, browser.act to click/fill, browser.eval for JS.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"url": {"type": "string"}}}`),
	}
}

func (c BrowserOpen) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.URL == "" {
		res, err := c.B.Info(ctx)
		if err != nil {
			return nil, internal(err)
		}
		return res, nil
	}
	res, err := c.B.Navigate(ctx, req.URL)
	if err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// --- browser.read ---

// BrowserRead returns the visible text of the current page.
type BrowserRead struct{ B *browser.Browser }

func (BrowserRead) Name() string { return "browser.read" }

func (BrowserRead) Describe() Descriptor {
	return Descriptor{
		Name: "browser.read",
		Description: "Read the current page's visible text (not a screenshot: far fewer tokens, and you get the actual words). " +
			"max_chars caps the length (default 20000).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"max_chars": {"type": "integer"}}}`),
	}
}

func (c BrowserRead) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Max int `json:"max_chars"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	res, err := c.B.Read(ctx, req.Max)
	if err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// --- browser.act ---

// BrowserAct clicks or fills an element.
type BrowserAct struct{ B *browser.Browser }

func (BrowserAct) Name() string { return "browser.act" }

func (BrowserAct) Describe() Descriptor {
	return Descriptor{
		Name: "browser.act",
		Description: "Click or fill an element on the current page. Identify it with selector (a CSS selector) or text (its " +
			"exact or partial visible label). action=click (default) clicks it; action=fill types value into it (an input). " +
			"Example: {\"text\":\"Sign in\"} or {\"selector\":\"#email\",\"action\":\"fill\",\"value\":\"a@b.com\"}.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"selector": {"type": "string", "description": "CSS selector."},
				"text": {"type": "string", "description": "Visible text of the element (exact preferred)."},
				"action": {"type": "string", "enum": ["click", "fill"], "description": "Default click."},
				"value": {"type": "string", "description": "Text to type when action=fill."}
			}
		}`),
	}
}

func (c BrowserAct) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Selector string `json:"selector"`
		Text     string `json:"text"`
		Action   string `json:"action"`
		Value    string `json:"value"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Selector == "" && req.Text == "" {
		return nil, badRequestMsg("give a selector or text")
	}
	if req.Action == "" {
		req.Action = "click"
	}
	res, err := c.B.Act(ctx, req.Action, req.Selector, req.Text, req.Value)
	if err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// --- browser.eval ---

// BrowserEval runs JavaScript in the page.
type BrowserEval struct{ B *browser.Browser }

func (BrowserEval) Name() string { return "browser.eval" }

func (BrowserEval) Describe() Descriptor {
	return Descriptor{
		Name: "browser.eval",
		Description: "Run a JavaScript expression in the current page and return its value (JSON). Use for anything the other " +
			"browser tools do not cover: read a specific element, submit a form, scrape a table. Promises are awaited.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"expr": {"type": "string"}}, "required": ["expr"]}`),
	}
}

func (c BrowserEval) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Expr string `json:"expr"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	val, err := c.B.Eval(ctx, req.Expr)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"value": json.RawMessage(val)}, nil
}

// --- browser.screenshot ---

// BrowserShot captures the page as an image.
type BrowserShot struct{ B *browser.Browser }

func (BrowserShot) Name() string { return "browser.screenshot" }

func (BrowserShot) Describe() Descriptor {
	return Descriptor{
		Name:        "browser.screenshot",
		Description: "Capture the current browser page as a PNG image (for when the text is not enough).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (c BrowserShot) Invoke(ctx context.Context, _ json.RawMessage, _ Emit) (any, error) {
	data, err := c.B.Screenshot(ctx)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"images": []map[string]any{{"mime_type": "image/png", "data": data}}}, nil
}

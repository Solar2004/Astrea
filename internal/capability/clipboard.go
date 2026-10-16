package capability

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// clipBackend reads and writes the desktop clipboard. It must run inside the
// user's desktop session (gui helper or full agent).
type clipBackend interface {
	// Text returns the clipboard text; image reports a picture with no text.
	Text() (text string, image bool, err error)
	SetText(text string) error
	// Seq changes whenever the clipboard content changes.
	Seq() uint64
}

var clip clipBackend

func requireClip() error {
	if clip == nil {
		return &protocol.Error{Code: protocol.CodeUnavailable, Message: "clipboard not supported on this OS build (Linux needs wl-clipboard or xclip)"}
	}
	return nil
}

// ClipEntry is one remembered clipboard content.
type ClipEntry struct {
	Time  string `json:"time"`
	Kind  string `json:"kind"` // text | image
	Text  string `json:"text,omitempty"`
	Chars int    `json:"chars,omitempty"`
	// Process is the focused program when it was copied (who copied it).
	Process string `json:"process,omitempty"`
	// Astrea: it was put there by clipboard.set, not by a person.
	Astrea bool `json:"astrea,omitempty"`
}

const (
	clipKeep    = 200
	clipMaxText = 16 << 10
)

// ClipHistory watches the clipboard and remembers what passed through it.
type ClipHistory struct {
	Path string `json:"-"`

	mu      sync.Mutex
	Entries []ClipEntry `json:"entries"`
	Paused  bool        `json:"paused,omitempty"`
	ours    string      // last text we set, to tag it as Astrea's
	started bool
}

// Clipboard is the history of this process (gui helper or full agent).
var Clipboard = &ClipHistory{}

// Start loads the history and polls the clipboard until ctx is done.
func (h *ClipHistory) Start(ctx context.Context, path string) {
	if clip == nil {
		return
	}
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return
	}
	h.started, h.Path = true, path
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, h)
	}
	h.mu.Unlock()
	go func() {
		last := clip.Seq()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if s := clip.Seq(); s != last {
				last = s
				h.capture()
			}
		}
	}()
}

func (h *ClipHistory) capture() {
	h.mu.Lock()
	paused := h.Paused
	h.mu.Unlock()
	if paused {
		return
	}
	text, image, err := clip.Text()
	if err != nil || (text == "" && !image) {
		return
	}
	e := ClipEntry{Time: time.Now().Format(time.RFC3339), Kind: "text", Chars: len([]rune(text))}
	if image {
		e.Kind = "image"
	} else {
		e.Text = text
		if len(e.Text) > clipMaxText {
			e.Text = e.Text[:clipMaxText] + "…"
		}
	}
	if idle, err := power.UserIdle(time.Minute); err == nil {
		e.Process = idle.Process
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !image && text == h.ours {
		e.Astrea = true
	}
	// Copying the same thing twice is one entry, moved to the top.
	if n := len(h.Entries); n > 0 && h.Entries[n-1].Kind == e.Kind && h.Entries[n-1].Text == e.Text {
		h.Entries = h.Entries[:n-1]
	}
	h.Entries = append(h.Entries, e)
	if len(h.Entries) > clipKeep {
		h.Entries = h.Entries[len(h.Entries)-clipKeep:]
	}
	h.saveLocked()
}

func (h *ClipHistory) saveLocked() {
	if h.Path == "" {
		return
	}
	if b, err := json.Marshal(h); err == nil {
		_ = os.WriteFile(h.Path, b, 0o600)
	}
}

// --- clipboard.get ---

// ClipboardGet returns the current clipboard text.
type ClipboardGet struct{}

func (ClipboardGet) Name() string { return "clipboard.get" }

func (ClipboardGet) Describe() Descriptor {
	return Descriptor{
		Name:        "clipboard.get",
		Description: "Read the text currently on the controlled PC's clipboard (kind=image if it holds a picture).",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (ClipboardGet) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	if err := requireClip(); err != nil {
		return nil, err
	}
	text, image, err := clip.Text()
	if err != nil {
		return nil, internal(err)
	}
	if image {
		return map[string]any{"kind": "image"}, nil
	}
	return map[string]any{"kind": "text", "text": text, "chars": len([]rune(text))}, nil
}

// --- clipboard.set ---

// ClipboardSet puts text on the clipboard.
type ClipboardSet struct{}

func (ClipboardSet) Name() string { return "clipboard.set" }

func (ClipboardSet) Describe() Descriptor {
	return Descriptor{
		Name: "clipboard.set",
		Description: "Put text on the controlled PC's clipboard. Faster and safer than key.type for long text: set it, " +
			"then key.press ctrl+v in the target field.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}`),
	}
}

func (ClipboardSet) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireClip(); err != nil {
		return nil, err
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	Clipboard.mu.Lock()
	Clipboard.ours = req.Text
	Clipboard.mu.Unlock()
	if err := clip.SetText(req.Text); err != nil {
		return nil, internal(err)
	}
	return map[string]any{"set": true, "chars": len([]rune(req.Text))}, nil
}

// --- clipboard.history ---

// ClipboardHistory lists what was copied on the PC.
type ClipboardHistory struct{}

func (ClipboardHistory) Name() string { return "clipboard.history" }

func (ClipboardHistory) Describe() Descriptor {
	return Descriptor{
		Name: "clipboard.history",
		Description: "What was copied on the controlled PC, newest first: time, text, and the program in focus when it " +
			"was copied (process). Keeps the last 200 copies (text up to 16 KB each; images are noted, not stored). " +
			"q filters by text or program; clear=true empties it; paused=true/false stops/resumes recording.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"limit": {"type": "integer", "description": "Default 20."},
				"q": {"type": "string", "description": "Case-insensitive filter on text or process."},
				"since": {"type": "string", "description": "RFC3339: only copies after this."},
				"clear": {"type": "boolean"},
				"paused": {"type": "boolean"}
			}
		}`),
	}
}

func (ClipboardHistory) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if err := requireClip(); err != nil {
		return nil, err
	}
	var req struct {
		Limit  int    `json:"limit"`
		Q      string `json:"q"`
		Since  string `json:"since"`
		Clear  bool   `json:"clear"`
		Paused *bool  `json:"paused"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	h := Clipboard
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.Clear {
		h.Entries = nil
	}
	if req.Paused != nil {
		h.Paused = *req.Paused
	}
	if req.Clear || req.Paused != nil {
		h.saveLocked()
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	q := strings.ToLower(req.Q)
	since, _ := time.Parse(time.RFC3339, req.Since)
	out := []ClipEntry{}
	for i := len(h.Entries) - 1; i >= 0 && len(out) < req.Limit; i-- {
		e := h.Entries[i]
		if t, _ := time.Parse(time.RFC3339, e.Time); !since.IsZero() && !t.After(since) {
			break
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Text), q) && !strings.Contains(strings.ToLower(e.Process), q) {
			continue
		}
		out = append(out, e)
	}
	return map[string]any{"entries": out, "total": len(h.Entries), "paused": h.Paused, "watching": h.started}, nil
}

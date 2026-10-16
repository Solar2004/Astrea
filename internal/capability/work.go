package capability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/work"
)

// RegisterWork registers the work.* capabilities: declaring, reporting and
// reading the tasks the AI works on here. auditPath ("" = disabled) feeds
// work.get with the actions each task actually ran.
// shotDir ("" = disabled) keeps a screenshot of every task that ends failed.
func RegisterWork(r *Registry, s *work.Store, auditPath, shotDir string) {
	shots := &Shots{Dir: shotDir, Reg: r}
	r.Register(WorkStart{Store: s})
	r.Register(WorkUpdate{Store: s})
	r.Register(WorkEnd{Store: s, Shots: shots})
	r.Register(WorkList{Store: s})
	r.Register(WorkGet{Store: s, AuditPath: auditPath, Shots: shots})
	r.Register(WorkShot{Shots: shots})
}

// Shots are the screenshots taken when a task fails: what was on screen when
// the AI gave up, for the owner to see later.
type Shots struct {
	Dir string
	Reg *Registry
}

const keepShots = 50

func (s *Shots) path(id string) string {
	if s == nil || s.Dir == "" || id == "" || strings.ContainsAny(id, `/\.`) {
		return ""
	}
	return filepath.Join(s.Dir, id+".jpg")
}

// Has reports whether task id has a failure screenshot.
func (s *Shots) Has(id string) bool {
	p := s.path(id)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// Take captures the screen for task id (best effort, in the background: the
// PC may be locked, asleep or without a desktop).
func (s *Shots) Take(id string) {
	p := s.path(id)
	if p == "" || s.Reg == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		res, err := s.Reg.Dispatch(ctx, "screen.capture", json.RawMessage(`{"max_width":1280,"quality":70,"format":"jpeg"}`), func(protocol.StreamChunk) {})
		if err != nil {
			return
		}
		raw, _ := json.Marshal(res)
		var out struct {
			Images []Image `json:"images"`
		}
		if json.Unmarshal(raw, &out) != nil || len(out.Images) == 0 {
			return
		}
		data, err := base64.StdEncoding.DecodeString(out.Images[0].Data)
		if err != nil || os.MkdirAll(s.Dir, 0o755) != nil {
			return
		}
		if os.WriteFile(p, data, 0o644) == nil {
			s.prune()
		}
	}()
}

func (s *Shots) prune() {
	files, _ := filepath.Glob(filepath.Join(s.Dir, "*.jpg"))
	if len(files) <= keepShots {
		return
	}
	type f struct {
		path string
		mod  time.Time
	}
	var all []f
	for _, p := range files {
		if st, err := os.Stat(p); err == nil {
			all = append(all, f{p, st.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for _, x := range all[min(keepShots, len(all)):] {
		os.Remove(x.path)
	}
}

// WorkShot returns the screenshot taken when a task failed.
type WorkShot struct{ Shots *Shots }

func (WorkShot) Name() string { return "work.shot" }

func (WorkShot) Describe() Descriptor {
	return Descriptor{
		Name:        "work.shot",
		Description: "The screenshot taken automatically when a task ended as failed (see work.get \"shot\": true).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"id": {"type": "string", "description": "task id"}},
			"required": ["id"]
		}`),
	}
}

func (w WorkShot) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ ID string }
	_ = json.Unmarshal(payload, &req)
	p := w.Shots.path(req.ID)
	data, err := os.ReadFile(p)
	if p == "" || err != nil {
		return nil, badRequest(errors.New("no screenshot for this task"))
	}
	return map[string]any{"id": req.ID, "images": []Image{{MimeType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data)}}}, nil
}

// WorkStart opens a task explicitly.
type WorkStart struct{ Store *work.Store }

func (WorkStart) Name() string { return "work.start" }

func (WorkStart) Describe() Descriptor {
	return Descriptor{
		Name: "work.start",
		Description: "Declare the task you are starting on this machine (shown to the owner, kept in the audit). " +
			"Closes the previous task as replaced. You can also open a task inline by passing " +
			`work {"title", "detail"} to any action.`,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "description": "what you are doing, a few words (e.g. \"Install Blender\", \"Run the tests of project X\")"},
				"detail": {"type": "string", "description": "why / for whom / what done looks like"}
			},
			"required": ["title"]
		}`),
	}
}

func (w WorkStart) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Title, Detail string }
	_ = json.Unmarshal(payload, &req)
	if strings.TrimSpace(req.Title) == "" {
		return nil, badRequest(errors.New(`"title" is required: what you are doing, in a few words`))
	}
	return w.Store.Start(req.Title, req.Detail), nil
}

// WorkUpdate reports progress on the current task.
type WorkUpdate struct{ Store *work.Store }

func (WorkUpdate) Name() string { return "work.update" }

func (WorkUpdate) Describe() Descriptor {
	return Descriptor{
		Name:        "work.update",
		Description: "Report progress on this machine's current task: a short note and/or a progress figure (\"3/5\", \"60%\").",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"note": {"type": "string"},
				"progress": {"type": "string", "description": "e.g. \"3/5\" or \"60%\""}
			}
		}`),
	}
}

func (w WorkUpdate) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Note, Progress string }
	_ = json.Unmarshal(payload, &req)
	t, err := w.Store.Update(req.Note, req.Progress)
	if err != nil {
		return nil, badRequest(err)
	}
	return t, nil
}

// WorkEnd closes the current task.
type WorkEnd struct {
	Store *work.Store
	Shots *Shots
}

func (WorkEnd) Name() string { return "work.end" }

func (WorkEnd) Describe() Descriptor {
	return Descriptor{
		Name:        "work.end",
		Description: "Close this machine's current task with its outcome and a short summary of what was done (files produced, results, what failed).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"status": {"type": "string", "enum": ["done", "failed", "cancelled"], "description": "default done"},
				"summary": {"type": "string"},
				"id": {"type": "string", "description": "optional: fail unless this is still the current task"}
			}
		}`),
	}
}

func (w WorkEnd) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ ID, Status, Summary string }
	_ = json.Unmarshal(payload, &req)
	t, err := w.Store.End(req.ID, req.Status, req.Summary)
	if err != nil {
		return nil, badRequest(err)
	}
	if t.Status == work.Failed {
		w.Shots.Take(t.ID)
	}
	return t, nil
}

// WorkList lists recent tasks, newest first, plus the current one.
type WorkList struct{ Store *work.Store }

func (WorkList) Name() string { return "work.list" }

func (WorkList) Describe() Descriptor {
	return Descriptor{
		Name:        "work.list",
		Description: "This machine's current task and the recent ones (newest first): title, status, progress, duration, number of actions.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"limit": {"type": "integer", "description": "default 20, max 300"}}
		}`),
	}
}

func (w WorkList) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct{ Limit int }
	_ = json.Unmarshal(payload, &req)
	if req.Limit <= 0 {
		req.Limit = 20
	}
	out := map[string]any{"tasks": w.Store.List(req.Limit)}
	if c, ok := w.Store.Current(); ok {
		out["current"] = c
	}
	return out, nil
}

// WorkGet returns one task with the actions that ran under it.
type WorkGet struct {
	Store     *work.Store
	AuditPath string
	Shots     *Shots
}

func (WorkGet) Name() string { return "work.get" }

func (WorkGet) Describe() Descriptor {
	return Descriptor{
		Name:        "work.get",
		Description: "One task in full: its notes and every action run under it (from the audit log, newest first).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string", "description": "task id (default: the current task)"},
				"limit": {"type": "integer", "description": "max actions (default 200, max 1000)"}
			}
		}`),
	}
}

func (w WorkGet) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID    string
		Limit int
	}
	_ = json.Unmarshal(payload, &req)
	if req.Limit <= 0 {
		req.Limit = 200
	}
	if req.Limit > 1000 {
		req.Limit = 1000
	}
	var t work.Task
	var ok bool
	if req.ID == "" {
		t, ok = w.Store.Current()
	} else {
		t, ok = w.Store.Get(req.ID)
	}
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "no such task"}
	}
	actions := []security.Entry{}
	if w.AuditPath != "" {
		if got, err := security.ReadAuditFunc(w.AuditPath, req.Limit, func(e security.Entry) bool { return e.Work == t.ID }); err == nil && got != nil {
			actions = got
		}
	}
	return map[string]any{"task": t, "actions": actions, "shot": w.Shots.Has(t.ID)}, nil
}

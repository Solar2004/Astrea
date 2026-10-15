// Package work keeps the machine's declared work: what the AI says it is
// doing here ("install Blender", "run the tests of project X").
//
// Every action that changes something arrives tagged with the task it belongs
// to — the current one ("continue") or a new one it opens — so the audit log
// can be read task by task, from what was actually done, not from what the AI
// says it did. One task is current at a time; opening a new one closes the
// previous as "replaced".
package work

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Task states.
const (
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
	Replaced  = "replaced"  // closed because another task was opened
	Abandoned = "abandoned" // no activity for StaleAfter
)

// StaleAfter closes a current task nobody has touched in this long.
const StaleAfter = 12 * time.Hour

// keep bounds the stored history.
const keep = 300

// Note is one progress report.
type Note struct {
	Time     time.Time `json:"time"`
	Text     string    `json:"text,omitempty"`
	Progress string    `json:"progress,omitempty"`
}

// Task is one declared piece of work.
type Task struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Detail   string     `json:"detail,omitempty"`
	Status   string     `json:"status"`
	Progress string     `json:"progress,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Started  time.Time  `json:"started"`
	Ended    *time.Time `json:"ended,omitempty"`
	LastSeen time.Time  `json:"last_activity"`
	Actions  int        `json:"actions"`
	Errors   int        `json:"errors"`
	Notes    []Note     `json:"notes,omitempty"`
}

// Spec is how a request declares its work: continue the current task or open
// a new one.
type Spec struct {
	Continue bool
	Title    string
	Detail   string
}

// ParseSpec accepts "continue", {"continue": true}, {"title", "detail"} and
// {"new": "title", "detail"}.
func ParseSpec(raw json.RawMessage) (Spec, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "continue", "current", "same":
			return Spec{Continue: true}, nil
		}
		return Spec{}, errors.New(`work must be "continue" or {"title": "...", "detail": "..."}`)
	}
	var o struct {
		Continue bool   `json:"continue"`
		New      string `json:"new"`
		Title    string `json:"title"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return Spec{}, errors.New(`work must be "continue" or {"title": "...", "detail": "..."}`)
	}
	if o.Title == "" {
		o.Title = o.New
	}
	if o.Continue && o.Title == "" {
		return Spec{Continue: true}, nil
	}
	if strings.TrimSpace(o.Title) == "" {
		return Spec{}, errors.New(`a new task needs a "title" (what you are doing, in a few words)`)
	}
	return Spec{Title: o.Title, Detail: o.Detail}, nil
}

// ErrNoCurrent: "continue" with no task open.
var ErrNoCurrent = errors.New(`no task is open on this machine: declare it with work {"title": "...", "detail": "..."}`)

// Store holds the tasks, persisted to a JSON file.
type Store struct {
	mu    sync.Mutex
	path  string
	tasks []*Task // oldest first
	dirty bool
	seq   int
	now   func() time.Time
}

// Open loads the store at path (missing or unreadable file = empty store).
func Open(path string) *Store {
	s := &Store{path: path, now: time.Now}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.tasks)
		s.seq = len(s.tasks)
	}
	return s
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// current returns the running task, closing it first if it went stale.
func (s *Store) current() *Task {
	if len(s.tasks) == 0 {
		return nil
	}
	t := s.tasks[len(s.tasks)-1]
	if t.Status != Running {
		return nil
	}
	if s.now().Sub(t.LastSeen) > StaleAfter {
		s.close(t, Abandoned, "")
		return nil
	}
	return t
}

func (s *Store) close(t *Task, status, summary string) {
	now := s.now()
	t.Status, t.Ended = status, &now
	if summary != "" {
		t.Summary = clip(summary, 2000)
	}
	s.dirty = true
}

func (s *Store) newID() string {
	s.seq++
	return s.now().UTC().Format("20060102-150405") + "-" + strconv.Itoa(s.seq)
}

// Start opens a new task, closing the current one as replaced.
func (s *Store) Start(title, detail string) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.start(title, detail)
	s.save()
	return *t
}

func (s *Store) start(title, detail string) *Task {
	if c := s.current(); c != nil {
		s.close(c, Replaced, "")
	}
	now := s.now()
	t := &Task{ID: s.newID(), Title: clip(title, 200), Detail: clip(detail, 2000), Status: Running, Started: now, LastSeen: now}
	s.tasks = append(s.tasks, t)
	if len(s.tasks) > keep {
		s.tasks = s.tasks[len(s.tasks)-keep:]
	}
	s.dirty = true
	return t
}

// Resolve turns a request's spec into the task it runs under, opening one if
// asked. opened reports a new task.
func (s *Store) Resolve(spec Spec) (t Task, opened bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if spec.Continue {
		c := s.current()
		if c == nil {
			if n := len(s.tasks); n > 0 {
				if l := s.tasks[n-1]; l.Status == Cancelled && l.Ended != nil && s.now().Sub(*l.Ended) < time.Hour {
					return Task{}, false, fmt.Errorf("the task %q was cancelled (%s): stop that work; open a new task only if you were asked to", l.Title, l.Summary)
				}
			}
			return Task{}, false, ErrNoCurrent
		}
		return *c, false, nil
	}
	c := s.start(spec.Title, spec.Detail)
	s.save()
	return *c, true, nil
}

// Count records one action done under task id.
func (s *Store) Count(id string, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.tasks) - 1; i >= 0; i-- {
		if t := s.tasks[i]; t.ID == id {
			t.Actions++
			if failed {
				t.Errors++
			}
			t.LastSeen = s.now()
			s.dirty = true
			return
		}
	}
}

// Update adds a progress note to the current task.
func (s *Store) Update(text, progress string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.current()
	if c == nil {
		return Task{}, ErrNoCurrent
	}
	n := Note{Time: s.now(), Text: clip(text, 1000), Progress: clip(progress, 40)}
	c.Notes = append(c.Notes, n)
	if len(c.Notes) > 200 {
		c.Notes = c.Notes[len(c.Notes)-200:]
	}
	if n.Progress != "" {
		c.Progress = n.Progress
	}
	c.LastSeen = n.Time
	s.dirty = true
	s.save()
	return *c, nil
}

// ErrNotCurrent: End for a task that is no longer the open one.
var ErrNotCurrent = errors.New("that task is no longer in progress")

// End closes the current task; id, if not empty, must be that task.
func (s *Store) End(id, status, summary string) (Task, error) {
	switch status {
	case "", "ok", "success":
		status = Done
	case Done, Failed, Cancelled:
	default:
		return Task{}, fmt.Errorf("status must be done, failed or cancelled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.current()
	if c == nil {
		return Task{}, ErrNoCurrent
	}
	if id != "" && c.ID != id {
		return Task{}, ErrNotCurrent
	}
	s.close(c, status, summary)
	s.save()
	return *c, nil
}

// Current returns the open task, if any.
func (s *Store) Current() (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.current()
	if c == nil {
		return Task{}, false
	}
	return *c, true
}

// Get returns one task by id.
func (s *Store) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current() // refresh staleness
	for _, t := range s.tasks {
		if t.ID == id {
			return *t, true
		}
	}
	return Task{}, false
}

// List returns up to limit tasks, newest first.
func (s *Store) List(limit int) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current()
	out := []Task{}
	for i := len(s.tasks) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *s.tasks[i])
	}
	return out
}

// Flush writes pending changes (action counts are saved lazily).
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.save()
}

func (s *Store) save() {
	if !s.dirty || s.path == "" {
		return
	}
	data, err := json.Marshal(s.tasks)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o640) == nil && os.Rename(tmp, s.path) == nil {
		s.dirty = false
	}
}

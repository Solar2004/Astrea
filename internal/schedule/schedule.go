// Package schedule runs capability calls at a time of day or every N minutes
// on the controlled machine: "lock this PC at 08:00 on weekdays", "run the
// task backup every 30 min", "open the class app at 07:55 once".
//
// It lives in the agent, not the panel, so a schedule keeps working while the
// panel or the tailnet is down. Times are the PC's local time. Entries persist
// in a JSON file. The package knows nothing about capabilities: the caller
// injects a Dispatch func (which goes through the guard and audit log).
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one scheduled call.
type Entry struct {
	Name string `json:"name"`
	// At is the local time of day, "HH:MM". Exclusive with Every.
	At string `json:"at,omitempty"`
	// Days limits At to these weekdays (mon…sun). Empty = every day.
	Days []string `json:"days,omitempty"`
	// Every repeats the call, e.g. "30m", "2h" (minimum 1m). Exclusive with At.
	Every string `json:"every,omitempty"`
	// Once deletes the entry after its first run.
	Once bool `json:"once,omitempty"`
	// CatchUpMin: an At run missed while the PC was off or the agent down
	// still fires if it is at most this many minutes late. Default 60; -1 never.
	CatchUpMin int `json:"catch_up_min,omitempty"`

	Call string          `json:"call"`
	Args json.RawMessage `json:"args,omitempty"`

	Disabled bool `json:"disabled,omitempty"`

	LastRun string `json:"last_run,omitempty"` // RFC3339
	LastErr string `json:"last_error,omitempty"`
	Created string `json:"created,omitempty"`
}

// Dispatch runs a capability by name.
type Dispatch func(ctx context.Context, method string, args json.RawMessage) (any, error)

var (
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)
	atRe   = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Validate checks an entry and normalizes its days.
func Validate(e *Entry) error {
	if !nameRe.MatchString(e.Name) {
		return fmt.Errorf("name must match %s", nameRe)
	}
	if e.Call == "" {
		return fmt.Errorf("call is required (a capability such as session.lock or task.<name>)")
	}
	if strings.HasPrefix(e.Call, "schedule.") {
		return fmt.Errorf("a schedule cannot call schedule.*")
	}
	if (e.At == "") == (e.Every == "") {
		return fmt.Errorf(`set exactly one of "at" (HH:MM) or "every" (e.g. 30m)`)
	}
	if e.At != "" && !atRe.MatchString(e.At) {
		return fmt.Errorf("at must be HH:MM (24 h), got %q", e.At)
	}
	if e.Every != "" {
		d, err := time.ParseDuration(e.Every)
		if err != nil || d < time.Minute {
			return fmt.Errorf("every must be a duration of at least 1m, e.g. 30m or 2h")
		}
		if len(e.Days) > 0 {
			return fmt.Errorf("days only applies to at")
		}
	}
	for i, d := range e.Days {
		d = strings.ToLower(d)
		if len(d) > 3 {
			d = d[:3]
		}
		if _, ok := weekdays[d]; !ok {
			return fmt.Errorf("unknown day %q (use mon, tue, wed, thu, fri, sat, sun)", e.Days[i])
		}
		e.Days[i] = d
	}
	if len(e.Args) > 0 {
		var obj map[string]any
		if json.Unmarshal(e.Args, &obj) != nil {
			return fmt.Errorf("args must be a JSON object")
		}
	}
	return nil
}

// Due reports whether e should run at now, and the slot it fires for (used as
// the run time so a late catch-up does not shift the next slot).
func (e *Entry) Due(now time.Time) (bool, time.Time) {
	if e.Disabled {
		return false, time.Time{}
	}
	last, _ := time.Parse(time.RFC3339, e.LastRun)
	if e.Every != "" {
		d, _ := time.ParseDuration(e.Every)
		if last.IsZero() {
			created, _ := time.Parse(time.RFC3339, e.Created)
			return !created.IsZero() && now.Sub(created) >= d, now
		}
		return now.Sub(last) >= d, now
	}
	var h, m int
	fmt.Sscanf(e.At, "%d:%d", &h, &m)
	slot := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if slot.After(now) {
		return false, time.Time{}
	}
	if !e.dayOK(slot.Weekday()) || !last.Before(slot) {
		return false, time.Time{}
	}
	created, _ := time.Parse(time.RFC3339, e.Created)
	if !created.IsZero() && created.After(slot) {
		return false, time.Time{} // defined after today's slot: first run is tomorrow
	}
	grace := e.CatchUpMin
	if grace == 0 {
		grace = 60
	}
	if grace < 0 {
		grace = 1 // only the minute it is due
	}
	if now.Sub(slot) > time.Duration(grace)*time.Minute {
		return false, time.Time{}
	}
	return true, slot
}

func (e *Entry) dayOK(d time.Weekday) bool {
	if len(e.Days) == 0 {
		return true
	}
	for _, s := range e.Days {
		if weekdays[s] == d {
			return true
		}
	}
	return false
}

// Next returns when e will run next after now (zero if never).
func (e *Entry) Next(now time.Time) time.Time {
	if e.Disabled {
		return time.Time{}
	}
	if e.Every != "" {
		d, _ := time.ParseDuration(e.Every)
		base, _ := time.Parse(time.RFC3339, e.LastRun)
		if base.IsZero() {
			base, _ = time.Parse(time.RFC3339, e.Created)
		}
		if base.IsZero() {
			base = now
		}
		return base.Add(d).Local()
	}
	var h, m int
	fmt.Sscanf(e.At, "%d:%d", &h, &m)
	for i := 0; i < 8; i++ {
		day := now.AddDate(0, 0, i)
		slot := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, now.Location())
		if slot.After(now) && e.dayOK(slot.Weekday()) {
			return slot
		}
	}
	return time.Time{}
}

// Store holds the entries and runs them.
type Store struct {
	path     string
	dispatch Dispatch
	logf     func(string, ...any)

	mu      sync.Mutex
	entries map[string]*Entry
}

// Open loads entries from path (missing file = none).
func Open(path string, dispatch Dispatch, logf func(string, ...any)) *Store {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Store{path: path, dispatch: dispatch, logf: logf, entries: map[string]*Entry{}}
	if b, err := os.ReadFile(path); err == nil {
		var list []*Entry
		if json.Unmarshal(b, &list) == nil {
			for _, e := range list {
				s.entries[e.Name] = e
			}
		}
	}
	return s
}

// Set validates and stores (or replaces) an entry.
func (s *Store) Set(e *Entry) error {
	if err := Validate(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[e.Name]; ok && old.Created != "" {
		e.Created = old.Created
	}
	if e.Created == "" {
		e.Created = time.Now().UTC().Format(time.RFC3339)
	}
	e.LastRun, e.LastErr = "", ""
	s.entries[e.Name] = e
	return s.saveLocked()
}

// Delete removes an entry; reports whether it existed.
func (s *Store) Delete(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[name]
	delete(s.entries, name)
	if ok {
		_ = s.saveLocked()
	}
	return ok
}

// View is an entry plus its next run, for listing.
type View struct {
	Entry
	NextRun string `json:"next_run,omitempty"`
}

// List returns all entries sorted by name.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]View, 0, len(s.entries))
	for _, e := range s.entries {
		v := View{Entry: *e}
		if n := e.Next(now); !n.IsZero() {
			v.NextRun = n.Format(time.RFC3339)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunNow fires an entry immediately (like a scheduled run).
func (s *Store) RunNow(ctx context.Context, name string) (any, error) {
	s.mu.Lock()
	e, ok := s.entries[name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such schedule %q", name)
	}
	return s.fire(ctx, e, time.Now())
}

// Run checks the entries every 15 s until ctx is done.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		s.tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Store) tick(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var due []*Entry
	var slots []time.Time
	for _, e := range s.entries {
		if ok, slot := e.Due(now); ok {
			due = append(due, e)
			slots = append(slots, slot)
		}
	}
	s.mu.Unlock()
	for i, e := range due {
		go s.fire(ctx, e, slots[i])
	}
}

func (s *Store) fire(ctx context.Context, e *Entry, slot time.Time) (any, error) {
	// Mark first: a slow call must not be fired again by the next tick.
	s.mu.Lock()
	e.LastRun = slot.UTC().Format(time.RFC3339)
	s.mu.Unlock()

	args := e.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := s.dispatch(cctx, e.Call, args)
	s.logf("schedule %s: %s → err=%v", e.Name, e.Call, err)

	s.mu.Lock()
	defer s.mu.Unlock()
	e.LastErr = ""
	if err != nil {
		e.LastErr = err.Error()
	}
	if e.Once {
		delete(s.entries, e.Name)
	}
	_ = s.saveLocked()
	return res, err
}

func (s *Store) saveLocked() error {
	list := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

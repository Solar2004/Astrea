// Package uptime records when this machine was on, asleep or off — kept by
// the agent itself, so the history has no holes when the panel or the hub
// were down. A heartbeat every minute extends the current span; a new boot or
// a gap between heartbeats (the machine slept) closes it. The OS's own
// records (oslog_*.go) fill in the time before the agent was installed.
//
// It also counts, per hour, the actions the AI ran here under a declared
// task: the "activity squares" of the panel.
package uptime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/host"
)

// How a span ended.
const (
	EndRunning = "running" // the current span: still on
	EndOff     = "off"     // shut down (or the agent could not tell)
	EndSleep   = "sleep"   // slept, hibernated or Windows "fast startup" shutdown
	EndCrash   = "crash"   // power cut, freeze or forced reset
)

const (
	beat     = time.Minute
	sleepGap = 3 * time.Minute // a longer silence of a running agent means the machine slept
	bootSkew = 90              // seconds; the boot time computed by the OS jitters a little
	keepFor  = 120 * 24 * time.Hour
)

// Span is one stretch of time the machine was on.
type Span struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	EndKind string    `json:"end_kind"`
	Source  string    `json:"source,omitempty"` // "os": rebuilt from the OS log, before the agent existed
}

type state struct {
	Boot     int64         `json:"boot"` // unix s of the boot the last span belongs to
	Spans    []Span        `json:"spans"`
	Activity map[int64]int `json:"activity"` // hour start (unix s) → AI actions
}

// Store is the on-disk uptime and activity history. Safe for concurrent use.
type Store struct {
	path string

	mu   sync.Mutex
	st   state
	last time.Time // wall clock of this process's last heartbeat

	osMu    sync.Mutex
	osSpans []Span
	osAt    time.Time
}

// Open loads the history at path (a missing or broken file starts empty).
func Open(path string) *Store {
	s := &Store{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.st)
	}
	if s.st.Activity == nil {
		s.st.Activity = map[int64]int{}
	}
	return s
}

// Run records a heartbeat now and every minute until ctx ends.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(beat)
	defer t.Stop()
	for {
		if boot, err := host.BootTimeWithContext(ctx); err == nil {
			s.beat(time.Now(), int64(boot))
		}
		select {
		case <-ctx.Done():
			s.flush()
			return
		case <-t.C:
		}
	}
}

func (s *Store) beat(now time.Time, boot int64) {
	now = now.Round(0).UTC() // wall clock: the monotonic one stops while asleep
	s.mu.Lock()
	n := len(s.st.Spans)
	switch {
	case n == 0 || abs(boot-s.st.Boot) > bootSkew:
		start := time.Unix(boot, 0).UTC()
		if n > 0 {
			last := &s.st.Spans[n-1]
			if last.EndKind == EndRunning {
				last.EndKind = endKindAfter(last.End)
			}
			if start.Before(last.End) {
				start = last.End
			}
		}
		s.st.Spans = append(s.st.Spans, Span{Start: start, End: now, EndKind: EndRunning})
		s.st.Boot = boot
	case !s.last.IsZero() && now.Sub(s.last) > sleepGap, s.st.Spans[n-1].EndKind != EndRunning:
		if last := &s.st.Spans[n-1]; last.EndKind == EndRunning {
			last.EndKind = EndSleep
		}
		s.st.Spans = append(s.st.Spans, Span{Start: now, End: now, EndKind: EndRunning})
	default:
		s.st.Spans[n-1].End = now
	}
	s.last = now
	s.pruneLocked(now)
	s.mu.Unlock()
	s.flush()
}

func (s *Store) pruneLocked(now time.Time) {
	cut := now.Add(-keepFor)
	i := 0
	for i < len(s.st.Spans) && s.st.Spans[i].End.Before(cut) {
		i++
	}
	s.st.Spans = s.st.Spans[i:]
	for h := range s.st.Activity {
		if time.Unix(h, 0).Before(cut) {
			delete(s.st.Activity, h)
		}
	}
}

func (s *Store) flush() {
	s.mu.Lock()
	data, err := json.Marshal(s.st)
	s.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// Count records one AI action at t (saved with the next heartbeat).
func (s *Store) Count(t time.Time) {
	h := t.UTC().Truncate(time.Hour).Unix()
	s.mu.Lock()
	s.st.Activity[h]++
	s.mu.Unlock()
}

// Hour is the AI activity of one hour.
type Hour struct {
	Hour    time.Time `json:"hour"`
	Actions int       `json:"actions"`
}

// Report is the history of the last days: spans oldest first (the OS log
// fills in what predates the agent), the current boot and AI activity.
type Report struct {
	Now       time.Time `json:"now"`
	Boot      time.Time `json:"boot"`
	Since     time.Time `json:"since"`      // first span recorded by the agent itself
	OSHistory bool      `json:"os_history"` // the OS log added older spans
	OnSec     int64     `json:"on_sec"`     // time on within the window
	Spans     []Span    `json:"spans"`
	Activity  []Hour    `json:"activity"`
}

// Report returns the last days of history.
func (s *Store) Report(ctx context.Context, days int) Report {
	now := time.Now().UTC()
	from := now.Add(-time.Duration(days) * 24 * time.Hour)

	s.mu.Lock()
	own := append([]Span(nil), s.st.Spans...)
	r := Report{Now: now, Boot: time.Unix(s.st.Boot, 0).UTC(), Spans: []Span{}, Activity: []Hour{}}
	for h, n := range s.st.Activity {
		if t := time.Unix(h, 0).UTC(); !t.Before(from) {
			r.Activity = append(r.Activity, Hour{t, n})
		}
	}
	s.mu.Unlock()
	sort.Slice(r.Activity, func(i, j int) bool { return r.Activity[i].Hour.Before(r.Activity[j].Hour) })

	firstOwn := now
	if len(own) > 0 {
		firstOwn = own[0].Start
		r.Since = firstOwn
	}
	var all []Span
	for _, sp := range s.fromOS(ctx) {
		if !sp.Start.Before(firstOwn) {
			continue
		}
		if sp.End.After(firstOwn) {
			sp.End = firstOwn
		}
		sp.Source = "os"
		all = append(all, sp)
		r.OSHistory = true
	}
	all = append(all, own...)
	for _, sp := range all {
		if sp.End.Before(from) {
			continue
		}
		if sp.Start.Before(from) {
			sp.Start = from
		}
		r.OnSec += int64(sp.End.Sub(sp.Start).Seconds())
		r.Spans = append(r.Spans, sp)
	}
	return r
}

// fromOS returns the OS's boot/sleep history, cached for 10 minutes (reading
// the Windows event log takes a couple of seconds).
func (s *Store) fromOS(ctx context.Context) []Span {
	s.osMu.Lock()
	defer s.osMu.Unlock()
	if s.osAt.IsZero() || time.Since(s.osAt) > 10*time.Minute {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		s.osSpans = osSpans(ctx, time.Now().Add(-keepFor))
		s.osAt = time.Now()
	}
	return s.osSpans
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

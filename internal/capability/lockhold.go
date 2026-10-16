package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/power"
)

// LockHold keeps a PC on the lock screen ("standby") while still letting the
// AI work on it: any desktop action signs back in by itself (the unlock gate),
// and once the AI and any person have been quiet for RelockSec, it locks again.
// Whoever walks up to the PC sees it locked; Astrea can still take over.
//
// The hold survives agent restarts (persisted to path).
type LockHold struct {
	Path string
	// Lock locks the desktop session now.
	Lock func(ctx context.Context) error
	// Idle reads sys.idle in the desktop session. An error means no session.
	Idle func(ctx context.Context) (power.Idle, error)
	Logf func(string, ...any)

	mu       sync.Mutex
	st       holdState
	lastAI   time.Time // last desktop action by the AI (or the owner from the panel)
	lastLock time.Time
	loaded   bool
}

type holdState struct {
	On bool `json:"on"`
	// Until ends the hold; zero = until released.
	Until     time.Time `json:"until,omitempty"`
	RelockSec int       `json:"relock_after_sec"`
	// Strict relocks even if a person is using the PC.
	Strict bool `json:"strict,omitempty"`
}

const defaultRelock = 120

// Touch records a desktop action (click, key, screenshot, gui.exec…), which
// postpones the relock.
func (h *LockHold) Touch() {
	h.mu.Lock()
	h.lastAI = time.Now()
	h.mu.Unlock()
}

func (h *LockHold) load() {
	if h.loaded {
		return
	}
	h.loaded = true
	if b, err := os.ReadFile(h.Path); err == nil {
		_ = json.Unmarshal(b, &h.st)
	}
}

func (h *LockHold) saveLocked() {
	if h.Path == "" {
		return
	}
	b, _ := json.Marshal(h.st)
	_ = os.WriteFile(h.Path, b, 0o600)
}

// Status reports the hold.
func (h *LockHold) Status() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	m := map[string]any{"hold": h.st.On}
	if h.st.On {
		m["relock_after_sec"] = h.st.RelockSec
		m["strict"] = h.st.Strict
		if !h.st.Until.IsZero() {
			m["until"] = h.st.Until.Format(time.RFC3339)
		}
	}
	return m
}

func (h *LockHold) logf(f string, a ...any) {
	if h.Logf != nil {
		h.Logf(f, a...)
	}
}

// Run relocks while the hold is on, until ctx is done.
func (h *LockHold) Run(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.check(ctx, time.Now())
	}
}

func (h *LockHold) check(ctx context.Context, now time.Time) {
	h.mu.Lock()
	h.load()
	st, lastAI, lastLock := h.st, h.lastAI, h.lastLock
	if st.On && !st.Until.IsZero() && !now.Before(st.Until) {
		h.st = holdState{}
		h.saveLocked()
		h.mu.Unlock()
		h.logf("lock hold: ended at %s", st.Until.Format("15:04"))
		return
	}
	h.mu.Unlock()
	if !st.On {
		return
	}
	quiet := time.Duration(st.RelockSec) * time.Second
	if now.Sub(lastAI) < quiet || now.Sub(lastLock) < time.Minute {
		return
	}
	idle, err := h.Idle(ctx)
	if err != nil || idle.Locked {
		return // nobody signed in, or already locked
	}
	if !st.Strict && (idle.UserActive || idle.IdleSec < quiet.Seconds()) {
		return // a person is using it: do not pull the chair from under them
	}
	if err := h.Lock(ctx); err != nil {
		h.logf("lock hold: relock failed: %v", err)
		return
	}
	h.mu.Lock()
	h.lastLock = now
	h.mu.Unlock()
	h.logf("lock hold: relocked (quiet for %s)", quiet)
}

var hhmm = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

// nextClock returns the next local time at HH:MM after now.
func nextClock(s string, now time.Time) (time.Time, error) {
	if !hhmm.MatchString(s) {
		return time.Time{}, fmt.Errorf("hold_until must be HH:MM (24 h), got %q", s)
	}
	var hh, mm int
	fmt.Sscanf(s, "%d:%d", &hh, &mm)
	t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// --- session.lock ---

// SessionLock locks the PC now and, optionally, keeps it locked (standby).
type SessionLock struct{ Hold *LockHold }

func (SessionLock) Name() string { return "session.lock" }

func (SessionLock) Describe() Descriptor {
	return Descriptor{
		Name: "session.lock",
		Description: "Lock this PC's screen now (works from the headless agent too, unlike power.action lock). Standby: with " +
			"hold_until (HH:MM) or hold=true the PC stays locked for whoever walks up to it, but you can still work on it: " +
			"any desktop action signs back in by itself, and after relock_after_sec (default 120) with no action from you " +
			"and nobody at the keyboard it locks again. strict=true relocks even if a person signed in. release=true ends " +
			"the hold (and leaves the PC as it is). status=true only reports. Pair with schedule.add to lock at a set time, " +
			"e.g. at 08:00 call session.lock {\"hold_until\":\"14:00\"}.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"hold_until": {"type": "string", "description": "Keep it locked until this local time HH:MM (next occurrence)."},
				"hold": {"type": "boolean", "description": "Keep it locked until release=true."},
				"relock_after_sec": {"type": "integer", "description": "Quiet time before relocking. Default 120, min 30."},
				"strict": {"type": "boolean", "description": "Relock even if a person is using the PC."},
				"release": {"type": "boolean", "description": "End the hold without locking."},
				"status": {"type": "boolean", "description": "Only report the hold."}
			}
		}`),
	}
}

func (c SessionLock) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		HoldUntil string `json:"hold_until"`
		Hold      bool   `json:"hold"`
		Relock    int    `json:"relock_after_sec"`
		Strict    bool   `json:"strict"`
		Release   bool   `json:"release"`
		Status    bool   `json:"status"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	h := c.Hold
	if req.Status {
		return h.Status(), nil
	}
	if req.Release {
		h.mu.Lock()
		h.load()
		h.st = holdState{}
		h.saveLocked()
		h.mu.Unlock()
		return h.Status(), nil
	}
	if req.HoldUntil != "" || req.Hold {
		st := holdState{On: true, RelockSec: req.Relock, Strict: req.Strict}
		if st.RelockSec <= 0 {
			st.RelockSec = defaultRelock
		}
		if st.RelockSec < 30 {
			st.RelockSec = 30
		}
		if req.HoldUntil != "" {
			t, err := nextClock(req.HoldUntil, time.Now())
			if err != nil {
				return nil, badRequest(err)
			}
			st.Until = t
		}
		h.mu.Lock()
		h.load()
		h.st = st
		h.saveLocked()
		h.mu.Unlock()
	}
	if err := h.Lock(ctx); err != nil {
		return nil, internal(err)
	}
	h.mu.Lock()
	h.lastLock = time.Now()
	h.lastAI = time.Time{} // the lock itself is not "work going on"
	h.mu.Unlock()
	res := h.Status()
	res["locked"] = true
	return res, nil
}

// DesktopLock is the plain "lock now" served inside the desktop session (the
// gui helper, or a full agent): LockWorkStation must run in the user's
// session, which the headless agent is not.
type DesktopLock struct{}

func (DesktopLock) Name() string { return "session.lock" }

func (DesktopLock) Describe() Descriptor {
	return Descriptor{Name: "session.lock", Description: "Lock the desktop session now.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`)}
}

func (DesktopLock) Invoke(context.Context, json.RawMessage, Emit) (any, error) {
	if out, err := power.Run("lock", 0); err != nil {
		return nil, internal(fmt.Errorf("%v: %s", err, out))
	}
	return map[string]any{"locked": true}, nil
}

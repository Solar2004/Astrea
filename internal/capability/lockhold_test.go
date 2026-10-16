package capability

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Solar2004/Astrea/internal/power"
)

func TestLockHold(t *testing.T) {
	locks := 0
	idle := power.Idle{IdleSec: 600}
	h := &LockHold{
		Path: filepath.Join(t.TempDir(), "lockhold.json"),
		Lock: func(context.Context) error { locks++; return nil },
		Idle: func(context.Context) (power.Idle, error) { return idle, nil },
	}
	c := SessionLock{Hold: h}
	ctx := context.Background()
	if _, err := c.Invoke(ctx, json.RawMessage(`{"hold":true}`), nil); err != nil {
		t.Fatal(err)
	}
	if locks != 1 {
		t.Fatalf("locks = %d after session.lock", locks)
	}
	now := time.Now()

	// The AI signed in and is working: no relock.
	h.Touch()
	h.lastLock = time.Time{}
	h.check(ctx, now.Add(30*time.Second))
	if locks != 1 {
		t.Fatal("relocked while the AI was working")
	}
	// A person is using it: no relock (not strict).
	idle = power.Idle{IdleSec: 5, UserActive: true}
	h.check(ctx, now.Add(5*time.Minute))
	if locks != 1 {
		t.Fatal("relocked under a person")
	}
	// Everyone quiet: relock.
	idle = power.Idle{IdleSec: 600}
	h.check(ctx, now.Add(5*time.Minute))
	if locks != 2 {
		t.Fatalf("did not relock when quiet (locks=%d)", locks)
	}
	// Already locked: nothing.
	idle = power.Idle{Locked: true}
	h.check(ctx, now.Add(10*time.Minute))
	if locks != 2 {
		t.Fatal("relocked a locked PC")
	}
	// Persisted, then released.
	h2 := &LockHold{Path: h.Path}
	if h2.Status()["hold"] != true {
		t.Fatal("hold not persisted")
	}
	if _, err := c.Invoke(ctx, json.RawMessage(`{"release":true}`), nil); err != nil {
		t.Fatal(err)
	}
	if h.Status()["hold"] != false || locks != 2 {
		t.Fatalf("release locked or kept the hold: %v %d", h.Status(), locks)
	}
}

func TestLockHoldEnds(t *testing.T) {
	h := &LockHold{Lock: func(context.Context) error { return nil },
		Idle: func(context.Context) (power.Idle, error) { return power.Idle{IdleSec: 999}, nil }}
	h.loaded = true
	h.st = holdState{On: true, Until: time.Now().Add(time.Minute), RelockSec: 120}
	h.check(context.Background(), time.Now().Add(2*time.Minute))
	if h.Status()["hold"] != false {
		t.Fatal("hold did not end at until")
	}
	if _, err := nextClock("8:5", time.Now()); err == nil {
		t.Fatal("accepted a bad time")
	}
	if n, _ := nextClock("00:00", time.Now()); !n.After(time.Now()) {
		t.Fatal("next clock in the past")
	}
}

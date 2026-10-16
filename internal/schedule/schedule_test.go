package schedule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func at(h, m int) time.Time { return time.Date(2026, 10, 16, h, m, 0, 0, time.Local) } // a Friday

func TestValidate(t *testing.T) {
	bad := []Entry{
		{Name: "x", Call: "session.lock"},                                   // no at/every
		{Name: "x", Call: "session.lock", At: "8:00", Every: "1h"},          // both
		{Name: "x", Call: "session.lock", At: "25:00"},                      // bad hour
		{Name: "x", Call: "session.lock", Every: "10s"},                     // too short
		{Name: "x", Call: "session.lock", At: "08:00", Days: []string{"x"}}, // bad day
		{Name: "x", Call: "schedule.add", At: "08:00"},                      // recursion
		{Name: "a b", Call: "exec", At: "08:00"},                            // bad name
	}
	for i, e := range bad {
		if err := Validate(&e); err == nil {
			t.Errorf("case %d: expected error for %+v", i, e)
		}
	}
	ok := Entry{Name: "lock8", Call: "session.lock", At: "08:00", Days: []string{"Monday", "fri"}}
	if err := Validate(&ok); err != nil {
		t.Fatal(err)
	}
	if ok.Days[0] != "mon" {
		t.Errorf("days not normalized: %v", ok.Days)
	}
}

func TestDueAt(t *testing.T) {
	e := Entry{Name: "x", Call: "c", At: "08:00", Created: at(7, 0).UTC().Format(time.RFC3339)}
	if ok, _ := e.Due(at(7, 59)); ok {
		t.Error("due before the slot")
	}
	ok, slot := e.Due(at(8, 0))
	if !ok || !slot.Equal(at(8, 0)) {
		t.Fatalf("not due at the slot: %v %v", ok, slot)
	}
	e.LastRun = slot.UTC().Format(time.RFC3339)
	if ok, _ := e.Due(at(8, 1)); ok {
		t.Error("fired twice for the same slot")
	}
	// Catch-up: agent was down at 08:00, comes back at 08:40 (inside 60 min).
	e.LastRun = ""
	if ok, _ := e.Due(at(8, 40)); !ok {
		t.Error("missed catch-up inside the grace window")
	}
	if ok, _ := e.Due(at(9, 30)); ok {
		t.Error("caught up outside the grace window")
	}
	// Defined after today's slot: not today.
	e.Created = at(9, 0).UTC().Format(time.RFC3339)
	if ok, _ := e.Due(at(9, 1)); ok {
		t.Error("fired for a slot before the entry existed")
	}
	// Wrong weekday (the 16th is a Friday).
	w := Entry{Name: "x", Call: "c", At: "08:00", Days: []string{"mon"}}
	if ok, _ := w.Due(at(8, 0)); ok {
		t.Error("fired on a day not in days")
	}
	if n := w.Next(at(8, 0)); n.Weekday() != time.Monday {
		t.Errorf("next run %v is not a Monday", n)
	}
}

func TestDueEvery(t *testing.T) {
	e := Entry{Name: "x", Call: "c", Every: "30m", Created: at(8, 0).UTC().Format(time.RFC3339)}
	if ok, _ := e.Due(at(8, 20)); ok {
		t.Error("due before the first interval")
	}
	if ok, _ := e.Due(at(8, 30)); !ok {
		t.Error("not due after the interval")
	}
	e.LastRun = at(8, 30).UTC().Format(time.RFC3339)
	if ok, _ := e.Due(at(8, 45)); ok {
		t.Error("due again too soon")
	}
}

func TestStoreFiresOnceAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedule.json")
	var mu sync.Mutex
	var calls []string
	d := func(_ context.Context, m string, a json.RawMessage) (any, error) {
		mu.Lock()
		calls = append(calls, m+string(a))
		mu.Unlock()
		return nil, nil
	}
	s := Open(path, d, nil)
	if err := s.Set(&Entry{Name: "once", Call: "session.lock", Args: json.RawMessage(`{"hold_until":"14:00"}`), At: "08:00", Once: true}); err != nil {
		t.Fatal(err)
	}
	// Created now; pretend it was created yesterday so today's 08:00 counts.
	s.entries["once"].Created = at(0, 0).AddDate(0, 0, -1).UTC().Format(time.RFC3339)
	if _, err := s.fire(context.Background(), s.entries["once"], at(8, 0)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != `session.lock{"hold_until":"14:00"}` {
		t.Fatalf("calls = %v", calls)
	}
	if len(s.List()) != 0 {
		t.Error("once entry not deleted after running")
	}
	if err := s.Set(&Entry{Name: "keep", Call: "exec", Every: "1h"}); err != nil {
		t.Fatal(err)
	}
	if got := Open(path, d, nil).List(); len(got) != 1 || got[0].Name != "keep" || got[0].NextRun == "" {
		t.Fatalf("not persisted: %+v", got)
	}
}

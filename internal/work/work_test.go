package work

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestParseSpec(t *testing.T) {
	for raw, want := range map[string]Spec{
		`"continue"`:                         {Continue: true},
		`{"continue": true}`:                 {Continue: true},
		`{"title": "Videos", "detail": "x"}`: {Title: "Videos", Detail: "x"},
		`{"new": "Tests"}`:                   {Title: "Tests"},
	} {
		got, err := ParseSpec(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", raw, got, err)
		}
	}
	for _, raw := range []string{`"whatever"`, `{}`, `{"title": "  "}`, `3`} {
		if _, err := ParseSpec(json.RawMessage(raw)); err == nil {
			t.Errorf("%s should fail", raw)
		}
	}
}

func TestLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.json")
	s := Open(path)
	if _, _, err := s.Resolve(Spec{Continue: true}); err != ErrNoCurrent {
		t.Fatalf("continue with nothing open: %v", err)
	}
	a, opened, err := s.Resolve(Spec{Title: "Videos"})
	if err != nil || !opened || a.Status != Running {
		t.Fatalf("open: %+v %v", a, err)
	}
	s.Count(a.ID, false)
	s.Count(a.ID, true)
	if c, _, _ := s.Resolve(Spec{Continue: true}); c.ID != a.ID || c.Actions != 2 || c.Errors != 1 {
		t.Fatalf("continue: %+v", c)
	}
	if _, err := s.Update("3 listos", "3/5"); err != nil {
		t.Fatal(err)
	}
	b := s.Start("Tests", "")
	if got, _ := s.Get(a.ID); got.Status != Replaced || got.Ended == nil {
		t.Fatalf("previous should be replaced: %+v", got)
	}
	if _, err := s.End("failed", "2 fallan"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Current(); ok {
		t.Fatal("nothing should be open after End")
	}
	s.Flush()

	// Persisted, newest first.
	r := Open(path)
	l := r.List(10)
	if len(l) != 2 || l[0].ID != b.ID || l[0].Status != Failed || l[0].Summary != "2 fallan" || l[1].Progress != "3/5" || l[1].Actions != 2 {
		t.Fatalf("reloaded: %+v", l)
	}
}

func TestStale(t *testing.T) {
	s := Open("")
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Start("Old", "")
	now = now.Add(StaleAfter + time.Minute)
	if _, ok := s.Current(); ok {
		t.Fatal("a task untouched for StaleAfter should be closed")
	}
	if l := s.List(1); l[0].Status != Abandoned {
		t.Fatalf("status: %s", l[0].Status)
	}
}

package uptime

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBeatSpans(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "uptime.json"))
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	boot := t0.Add(-5 * time.Minute).Unix()
	s.beat(t0, boot)
	s.beat(t0.Add(time.Minute), boot)
	s.beat(t0.Add(10*time.Minute), boot)   // gap: slept
	s.beat(t0.Add(11*time.Minute), boot+2) // jitter: same boot
	newBoot := t0.Add(2 * time.Hour).Unix()
	s.beat(t0.Add(2*time.Hour+time.Minute), newBoot)

	sp := s.st.Spans
	if len(sp) != 3 {
		t.Fatalf("spans = %+v", sp)
	}
	if sp[0].Start.Unix() != boot || sp[0].EndKind != EndSleep || !sp[0].End.Equal(t0.Add(time.Minute)) {
		t.Errorf("first = %+v", sp[0])
	}
	if sp[1].EndKind != EndOff || !sp[1].End.Equal(t0.Add(11*time.Minute)) {
		t.Errorf("second = %+v", sp[1])
	}
	if sp[2].Start.Unix() != newBoot || sp[2].EndKind != EndRunning {
		t.Errorf("third = %+v", sp[2])
	}

	// Reopened (agent restart, same boot): keeps extending the same span.
	s.flush()
	s2 := Open(s.path)
	s2.beat(t0.Add(3*time.Hour), newBoot)
	if n := len(s2.st.Spans); n != 3 || !s2.st.Spans[2].End.Equal(t0.Add(3*time.Hour)) {
		t.Errorf("after restart = %+v", s2.st.Spans)
	}
}

func TestWindowsEvents(t *testing.T) {
	out := `6005 2026-10-01T08:00:00.0000000Z
42 2026-10-01T12:00:00.0000000Z
107 2026-10-01T13:00:00.0000000Z
1 2026-10-01T13:00:05.0000000Z
6006 2026-10-01T18:00:00.0000000Z
6005 2026-10-02T08:00:00.0000000Z
6005 2026-10-03T08:00:00.0000000Z
6008 2026-10-03T08:00:10.0000000Z
41 2026-10-03T08:00:11.0000000Z
`
	sp := spansFromEvents(parseWindowsEvents(out))
	want := []string{EndSleep, EndOff, EndCrash, EndRunning}
	if len(sp) != len(want) {
		t.Fatalf("spans = %+v", sp)
	}
	for i, k := range want {
		if sp[i].EndKind != k {
			t.Errorf("span %d = %+v, want %s", i, sp[i], k)
		}
	}
	if !sp[1].Start.Equal(time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("wake span start = %v", sp[1].Start)
	}
}

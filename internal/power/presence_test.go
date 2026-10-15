package power

import (
	"testing"
	"time"
)

// step is one poll: at seconds from t0, the OS reports idle seconds since
// the last input; ours means Astrea injected that input.
type step struct {
	at, idle float64
	ours     bool
}

func TestPresence(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 23, 0, 0, 0, time.UTC)
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	cases := []struct {
		name   string
		steps  []step
		active bool
	}{
		{"person using it", []step{{0, 300, false}, {2, 0, false}, {10, 1, false}, {20, 0, false}}, true},
		{"single touch (screen woke up, desk bump)", []step{{0, 300, false}, {2, 0, false}, {10, 8, false}, {60, 58, false}}, false},
		{"app left open, nobody touching", []step{{0, 900, false}, {2, 902, false}, {60, 960, false}}, false},
		{"Astrea's own clicks", []step{{0, 300, false}, {2, 0, true}, {10, 0, true}, {12, 2, false}}, false},
		{"person and Astrea", []step{{0, 300, false}, {2, 0, false}, {10, 1, false}, {32, 0, true}}, true},
		{"used long ago", []step{{0, 300, false}, {2, 0, false}, {10, 1, false}, {200, 191, false}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var p presence
			var now time.Time
			for _, s := range c.steps {
				now = t0.Add(sec(s.at))
				if s.ours {
					p.injected = now.Add(-sec(s.idle))
				}
				p.sample(now, sec(s.idle))
			}
			if _, active, why := p.verdict(now, 120*time.Second); active != c.active {
				t.Fatalf("active = %v (%s), want %v", active, why, c.active)
			}
		})
	}
}

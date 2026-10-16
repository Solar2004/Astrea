package uptime

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// osEvent is one power event from the OS log.
type osEvent struct {
	At   time.Time
	Kind string // up, off, sleep, crash
}

// windowsKind maps the System log event ids the agent asks for.
func windowsKind(id int) string {
	switch id {
	case 6005, 107, 1: // event log started (boot); resumed (Kernel-Power, Power-Troubleshooter)
		return "up"
	case 6006: // event log stopped: clean shutdown
		return EndOff
	case 42: // entering sleep
		return EndSleep
	case 41, 6008: // logged at the next boot: the previous one ended badly
		return EndCrash
	}
	return ""
}

// parseWindowsEvents reads "<id> <RFC3339 time>" lines.
func parseWindowsEvents(out string) []osEvent {
	var evs []osEvent
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f[1])
		if k := windowsKind(id); err == nil && k != "" {
			evs = append(evs, osEvent{t.UTC(), k})
		}
	}
	return evs
}

// spansFromEvents rebuilds on-spans from power events. A span with no clean
// end (the next thing seen is a boot) ended in a crash at its last known event.
func spansFromEvents(evs []osEvent) []Span {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].At.Before(evs[j].At) })
	var out []Span
	var cur *Span
	closeCur := func(at time.Time, kind string) {
		if cur != nil {
			if at.After(cur.End) {
				cur.End = at
			}
			cur.EndKind = kind
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, e := range evs {
		switch e.Kind {
		case "up":
			if cur != nil {
				if e.At.Sub(cur.End) < 2*time.Minute {
					cur.End = e.At // the second "resumed" event of the same wake
					continue
				}
				closeCur(cur.End, EndCrash)
			}
			cur = &Span{Start: e.At, End: e.At}
		case EndOff, EndSleep:
			closeCur(e.At, e.Kind)
		case EndCrash:
			// Logged after the reboot: the span before the current one crashed.
			if n := len(out); n > 0 && cur != nil && out[n-1].End.Before(cur.Start) {
				out[n-1].EndKind = EndCrash
			}
		}
	}
	if cur != nil {
		cur.EndKind = EndRunning
		out = append(out, *cur)
	}
	return out
}

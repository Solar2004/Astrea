package power

import (
	"math"
	"sync"
	"time"
)

// tracker reads the raw mouse and keyboard stream (low-level hooks, see
// hook_windows.go) and keeps only what a person does. The OS idle timer
// resets on anything: a mouse jiggler, a remote-desktop tool, an anti-AFK
// script, an optical sensor drifting on a glossy desk. Here input injected by
// software is dropped outright, and hardware mouse movement has to look like a
// hand: a stroke that travels, curves and speeds up and slows down, not a few
// pixels of noise, a ruler-straight constant-speed line, or the same move
// repeated like clockwork. Keys are only timed: which key was pressed is never
// read.
type tracker struct {
	mu      sync.Mutex
	started time.Time

	cur      stroke // stroke in progress
	inStroke bool
	strokes  []stroke    // finished strokes, oldest first
	acts     []time.Time // hardware key-ups, button-ups and wheel notches

	lastAny       time.Time // any event, injected or not (hook health)
	lastSynthetic time.Time // last injected event
	lastHW        time.Time // last hardware event, even if not human-looking
}

// stroke is one continuous mouse movement (no pause longer than strokeGap).
type stroke struct {
	start, end time.Time
	n          int        // samples
	x0, y0     float64    // first point
	x, y       float64    // last point
	path       float64    // distance travelled
	sum, sumSq float64    // per-sample step length, for the speed variation
	peak       float64    // longest step (peak speed)
	tail       [3]float64 // last three steps, newest last (end speed)
	dx, dy     float64    // previous step, for turns
	turn       float64    // signed turning, radians
	reversals  int        // steps that went back the way they came
}

const (
	strokeGap   = 300 * time.Millisecond
	maxStrokes  = 4000
	maxActs     = 4000
	minPath     = 12               // px: below this it is sensor noise or a desk bump
	ruler       = 0.985            // net/path above this is a perfectly straight line...
	steadySpeed = 0.12             // ...and step-length variation below this is a machine
	clockwork   = 0.08             // interval variation below this is a timer, not a person
	endRatio    = 0.6              // a hand slows down before it stops (≤0.25 of peak typically)
	nonstop     = 20 * time.Second // hands pause; a motor (wheel jiggler) does not
)

func (t *tracker) seen(at time.Time, injected bool) {
	t.lastAny = at
	if injected {
		t.lastSynthetic = at
	} else {
		t.lastHW = at
	}
}

// move records a mouse position.
func (t *tracker) move(at time.Time, x, y float64, injected bool) {
	t.seen(at, injected)
	if injected {
		return
	}
	if t.inStroke && at.Sub(t.cur.end) > strokeGap {
		t.closeStroke()
	}
	if !t.inStroke {
		t.cur = stroke{start: at, end: at, n: 1, x0: x, y0: y, x: x, y: y}
		t.inStroke = true
		return
	}
	step := math.Hypot(x-t.cur.x, y-t.cur.y)
	if step == 0 {
		return
	}
	s := &t.cur
	px, py := s.x, s.y
	s.end, s.n, s.x, s.y = at, s.n+1, x, y
	s.path += step
	s.sum += step
	s.sumSq += step * step
	s.peak = max(s.peak, step)
	s.tail = [3]float64{s.tail[1], s.tail[2], step}
	dx, dy := x-px, y-py
	if s.dx != 0 || s.dy != 0 {
		a := math.Atan2(s.dx*dy-s.dy*dx, s.dx*dx+s.dy*dy) // angle between steps
		s.turn += a
		if math.Abs(a) > 2.5 { // ~145°: turned back on itself
			s.reversals++
		}
	}
	s.dx, s.dy = dx, dy
}

// act records a key release, a button release or a wheel notch.
func (t *tracker) act(at time.Time, injected bool) {
	t.seen(at, injected)
	if injected {
		return
	}
	t.acts = append(t.acts, at)
	if len(t.acts) > maxActs {
		t.acts = t.acts[len(t.acts)-maxActs:]
	}
}

func (t *tracker) closeStroke() {
	t.inStroke = false
	t.strokes = append(t.strokes, t.cur)
	if len(t.strokes) > maxStrokes {
		t.strokes = t.strokes[len(t.strokes)-maxStrokes:]
	}
}

// handLike: the stroke goes somewhere and is not a machine-drawn line.
func (s stroke) handLike() bool {
	steps := float64(s.n - 1)
	if s.path < minPath || steps < 2 {
		return false
	}
	net := math.Hypot(s.x-s.x0, s.y-s.y0)
	if net < minPath/2 {
		return false // shaking in place: a vibrating desk, not a hand going somewhere
	}
	mean := s.sum / steps
	cv := math.Sqrt(math.Max(s.sumSq/steps-mean*mean, 0)) / mean
	switch {
	case steps >= 4 && net/s.path > ruler && cv < steadySpeed:
		return false // ruler line at constant speed
	case steps >= 8 && (s.tail[0]+s.tail[1]+s.tail[2])/3 > endRatio*s.peak:
		return false // stopped dead at full speed
	case s.reversals >= 10 && float64(s.reversals) > 0.3*steps:
		return false // back and forth: an oscillating jiggler
	case math.Abs(s.turn) > 4*math.Pi:
		return false // two or more full loops: a wheel jiggler
	case s.end.Sub(s.start) > nonstop:
		return false // moving without a single pause
	}
	return true
}

// periodic: four or more events at evenly spaced times (a jiggler's timer).
func periodic(ts []time.Time) bool {
	if len(ts) < 4 {
		return false
	}
	ts = ts[max(0, len(ts)-8):]
	var sum, sumSq float64
	for i := 1; i < len(ts); i++ {
		d := ts[i].Sub(ts[i-1]).Seconds()
		sum += d
		sumSq += d * d
	}
	k := float64(len(ts) - 1)
	mean := sum / k
	if mean < 2 { // typing and scrolling are fast and uneven, not a timer
		return false
	}
	return math.Sqrt(math.Max(sumSq/k-mean*mean, 0))/mean < clockwork
}

// evidence returns the moments of human input, oldest first, after dropping
// noise, ruler lines and clockwork, and prunes old history.
func (t *tracker) evidence(now time.Time) []time.Time {
	if t.inStroke && now.Sub(t.cur.end) > strokeGap {
		t.closeStroke()
	}
	for len(t.strokes) > 0 && now.Sub(t.strokes[0].end) > keepHistory {
		t.strokes = t.strokes[1:]
	}
	for len(t.acts) > 0 && now.Sub(t.acts[0]) > keepHistory {
		t.acts = t.acts[1:]
	}
	var moves []time.Time
	for _, s := range t.strokes {
		if s.handLike() {
			moves = append(moves, s.start)
		}
	}
	if t.inStroke && t.cur.handLike() {
		moves = append(moves, t.cur.start)
	}
	if periodic(moves) {
		moves = nil
	}
	acts := t.acts
	if periodic(acts) {
		acts = nil
	}
	return merge(moves, acts)
}

// merge two sorted lists into moments: inputs closer than sameMoment are one.
func merge(a, b []time.Time) []time.Time {
	var out []time.Time
	add := func(at time.Time) {
		if n := len(out); n > 0 && at.Sub(out[n-1]) < sameMoment {
			out[n-1] = at
			return
		}
		out = append(out, at)
	}
	for len(a) > 0 || len(b) > 0 {
		if len(b) == 0 || (len(a) > 0 && a[0].Before(b[0])) {
			add(a[0])
			a = a[1:]
		} else {
			add(b[0])
			b = b[1:]
		}
	}
	return out
}

// verdict: time since a person last did something, and whether there were
// two separate moments of it inside the threshold.
func (t *tracker) verdict(now time.Time, threshold time.Duration) (idle time.Duration, active bool, why string) {
	human := t.evidence(now)
	n := len(human)
	if n > 0 {
		idle = now.Sub(human[n-1])
	} else {
		idle = now.Sub(t.started)
	}
	switch {
	case n > 0 && idle < threshold:
		if n >= 2 && now.Sub(human[n-2]) < threshold {
			return idle, true, "keyboard/mouse in use"
		}
		return idle, false, "a single touch, not someone using it"
	case !t.lastSynthetic.IsZero() && now.Sub(t.lastSynthetic) < threshold:
		return idle, false, "only software moving the mouse/keyboard"
	case !t.lastHW.IsZero() && now.Sub(t.lastHW) < threshold:
		return idle, false, "mechanical movement, not a hand"
	case n > 0:
		return idle, false, "no input from a person recently"
	}
	return idle, false, "no input from a person"
}

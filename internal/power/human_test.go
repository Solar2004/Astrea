package power

import (
	"math"
	"testing"
	"time"
)

// hand draws a curved stroke from (x,y) that accelerates then slows down,
// sampled every 8 ms like a USB mouse.
func hand(t *tracker, at time.Time, x, y, dx, dy float64) {
	const n = 30
	for i := 0; i <= n; i++ {
		f := float64(i) / n
		e := (1 - math.Cos(f*math.Pi)) / 2 // ease in/out: bell-shaped speed
		bow := math.Sin(f*math.Pi) * 25    // the wrist makes it an arc
		t.move(at.Add(time.Duration(i)*8*time.Millisecond), x+dx*e+bow, y+dy*e-bow, false)
	}
}

// line draws a ruler-straight stroke at constant speed.
func line(t *tracker, at time.Time, x, y, dx, dy float64, injected bool) {
	const n = 20
	for i := 0; i <= n; i++ {
		f := float64(i) / n
		t.move(at.Add(time.Duration(i)*8*time.Millisecond), x+dx*f, y+dy*f, injected)
	}
}

// circles draws loops around (cx,cy) for secs seconds at 125 Hz, like a
// mouse sitting on a spinning wheel jiggler.
func circles(t *tracker, at time.Time, cx, cy, secs float64) {
	for i := 0; float64(i)*0.008 < secs; i++ {
		a := float64(i) * 0.05
		t.move(at.Add(time.Duration(i)*8*time.Millisecond), cx+80*math.Cos(a), cy+80*math.Sin(a), false)
	}
}

func TestTracker(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	cases := []struct {
		name   string
		play   func(tr *tracker)
		end    float64 // seconds from t0 when the verdict is asked
		active bool
		why    string
	}{
		{"person moving and clicking", func(tr *tracker) {
			hand(tr, at(300), 100, 100, 400, 150)
			tr.act(at(301), false)
			hand(tr, at(317), 500, 250, -200, 300)
			tr.act(at(318.4), false)
		}, 330, true, "keyboard/mouse in use"},
		{"person typing", func(tr *tracker) {
			for _, s := range []float64{300, 300.2, 300.5, 300.6, 305, 305.3, 311, 311.1} {
				tr.act(at(s), false)
			}
		}, 320, true, "keyboard/mouse in use"},
		{"software jiggler (SendInput)", func(tr *tracker) {
			for s := 0.0; s < 600; s += 30 {
				line(tr, at(s), 500, 500, 300, 0, true)
			}
		}, 600, false, "only software moving the mouse/keyboard"},
		{"USB jiggler, 1 px nudges", func(tr *tracker) {
			for s := 0.0; s < 600; s += 30 {
				tr.move(at(s), 500, 500, false)
				tr.move(at(s+0.01), 501, 500, false)
				tr.move(at(s+0.02), 500, 500, false)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"USB jiggler, straight lines", func(tr *tracker) {
			for s := 0.0; s < 600; s += 37 {
				line(tr, at(s), 500, 500, 200, 120, false)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"USB jiggler, curved but on a timer", func(tr *tracker) {
			for s := 0.0; s < 600; s += 20 {
				hand(tr, at(s), 400, 400, 150, 60)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"key pressed by a timer (F15 trick)", func(tr *tracker) {
			for s := 0.0; s < 600; s += 59 {
				tr.act(at(s), false)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"sensor drift on a glossy desk", func(tr *tracker) {
			x := 800.0
			for s := 0.0; s < 600; s += 7.3 {
				tr.move(at(s), x, 300, false)
				tr.move(at(s+0.008), x+1, 301, false)
				tr.move(at(s+0.016), x, 301, false)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"mouse on a vibrating table", func(tr *tracker) {
			for _, s := range []float64{480, 497.3, 503, 530.9, 541, 566.2, 590} {
				for i := 0; i < 60; i++ { // 60 ticks of ±2 px shaking in place
					tr.move(at(s+float64(i)*0.01), 300+float64(i%2)*2, 300+float64(i%3), false)
				}
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"wheel jiggler spinning the mouse", func(tr *tracker) {
			for _, s := range []float64{470, 503, 541, 566} {
				circles(tr, at(s), 500, 500, 6)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"motor moving it nonstop", func(tr *tracker) {
			for i := 0; i < 600*125; i++ { // slow wandering curve, never pausing
				f := float64(i) / 125
				tr.move(at(f), 500+200*math.Sin(f/7)+float64(i%3), 400+100*math.Sin(f/11), false)
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"oscillating jiggler", func(tr *tracker) {
			for _, s := range []float64{480, 497.3, 523, 541, 566.2, 590} {
				for i := 0; i < 40; i++ { // 15 px left, 15 px right, slightly diagonal
					tr.move(at(s+float64(i)*0.01), 400+float64(i%2)*15, 300+float64(i)*0.5, false)
				}
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"curve that stops dead at full speed", func(tr *tracker) {
			for _, s := range []float64{480, 497.3, 523, 541, 566.2, 590} {
				for i := 0; i <= 30; i++ {
					a := float64(i) * 0.04
					tr.move(at(s+float64(i)*0.008), 300+200*math.Sin(a), 300+200*(1-math.Cos(a)), false)
				}
			}
		}, 600, false, "mechanical movement, not a hand"},
		{"gamer: nonstop mouse plus keys", func(tr *tracker) {
			for i := 0; i < 120*125; i++ {
				f := float64(i) / 125
				tr.move(at(480+f), 500+200*math.Sin(f/3), 400+100*math.Sin(f/5), false)
			}
			for _, s := range []float64{482, 483.1, 490, 497.5, 520, 551, 580, 596} {
				tr.act(at(s), false)
			}
		}, 600, true, "keyboard/mouse in use"},
		{"one touch to wake the screen", func(tr *tracker) {
			hand(tr, at(590), 100, 100, 300, 50)
		}, 600, false, "a single touch, not someone using it"},
		{"app left open, nobody there", func(tr *tracker) {
			hand(tr, at(10), 100, 100, 300, 50)
			hand(tr, at(20), 400, 150, -100, 200)
		}, 600, false, "no input from a person recently"},
		{"person and a software jiggler together", func(tr *tracker) {
			for s := 0.0; s < 600; s += 30 {
				line(tr, at(s), 500, 500, 300, 0, true)
			}
			hand(tr, at(560), 100, 100, 400, 150)
			tr.act(at(575), false)
		}, 600, true, "keyboard/mouse in use"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &tracker{started: t0}
			c.play(tr)
			_, active, why := tr.verdict(at(c.end), 120*time.Second)
			if active != c.active || why != c.why {
				t.Fatalf("got active=%v %q, want %v %q", active, why, c.active, c.why)
			}
		})
	}
}

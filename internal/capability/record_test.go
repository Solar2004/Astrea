package capability

import (
	"encoding/json"
	"testing"
)

func TestBuildSteps(t *testing.T) {
	evs := []RecEvent{
		{T: 0, Kind: "move", X: 10, Y: 10},
		{T: 50, Kind: "move", X: 100, Y: 200},
		{T: 60, Kind: "down", X: 100, Y: 200, Btn: "left"},
		{T: 900, Kind: "type", Text: "hola"},
		{T: 1000, Kind: "key", Keys: "ctrl+s"},
		{T: 1100, Kind: "wheel", Dy: -3},
	}
	steps := buildSteps(evs, true)
	// Bare moves are dropped; the click self-positions (move+click), then a
	// sleep for the >400ms gap, the typed text, the combo and the scroll.
	var calls []string
	for _, s := range steps {
		calls = append(calls, s.Call)
	}
	want := []string{"mouse.move", "mouse.click", "sleep", "key.type", "key.press", "mouse.scroll"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("step %d = %q, want %q (all: %v)", i, calls[i], want[i], calls)
		}
	}
	// The sleep carries the real gap (900-60 = 840ms).
	var sleep struct {
		MS int `json:"ms"`
	}
	_ = json.Unmarshal(steps[2].Args, &sleep)
	if sleep.MS != 840 {
		t.Errorf("sleep ms = %d, want 840", sleep.MS)
	}
	// keep_moves keeps the first move too.
	if n := len(buildSteps(evs, false)); n != 8 {
		t.Errorf("keep_moves step count = %d, want 8", n)
	}
}

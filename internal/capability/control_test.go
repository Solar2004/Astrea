package capability

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestParseKeys(t *testing.T) {
	got, err := parseKeys(" Ctrl+Shift+Escape ")
	if err != nil || !reflect.DeepEqual(got, []string{"ctrl", "shift", "esc"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, ok := range []string{"a", "F5", "win+r", "alt+tab", "enter"} {
		if _, err := parseKeys(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"ctrl+ñ", "f13", "+", "ctrl++"} {
		if _, err := parseKeys(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

type nopInput struct{}

func (nopInput) MouseMove(int, int) error       { return nil }
func (nopInput) MouseClick(string, bool) error  { return nil }
func (nopInput) KeyType(string) error           { return nil }
func (nopInput) MouseButton(string, bool) error { return nil }
func (nopInput) Scroll(int, int) error          { return nil }
func (nopInput) KeyPress([]string) error        { return nil }

func TestHumanControl(t *testing.T) {
	old := input
	input = nopInput{}
	defer func() { input = old }()
	ctx := context.Background()
	on := func(v bool) {
		if _, err := (PanelControl{}).Invoke(ctx, json.RawMessage(`{"on":`+map[bool]string{true: "true", false: "false"}[v]+`}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	ai := json.RawMessage(`{"keys":"enter"}`)
	panel := json.RawMessage(`{"keys":"enter","from":"panel"}`)
	if _, err := (KeyPress{}).Invoke(ctx, ai, nil); err != nil {
		t.Fatalf("free: %v", err)
	}
	on(true)
	if _, err := (KeyPress{}).Invoke(ctx, ai, nil); !errors.Is(err, errHuman) {
		t.Fatalf("AI under human control: got %v", err)
	}
	if _, err := (KeyPress{}).Invoke(ctx, panel, nil); err != nil {
		t.Fatalf("panel under human control: %v", err)
	}
	on(false)
	if _, err := (MouseClick{}).Invoke(ctx, json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("released: %v", err)
	}
}

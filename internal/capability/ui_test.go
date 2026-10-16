package capability

import (
	"context"
	"encoding/json"
	"testing"
)

type fakeUI struct {
	els  []UIElement
	last map[int]UIElement
}

func (f *fakeUI) Snapshot(string) (string, []UIElement, error) {
	f.last = map[int]UIElement{}
	for _, e := range f.els {
		f.last[e.ID] = e
	}
	return "Test Window", f.els, nil
}
func (f *fakeUI) Center(id int) (int, int, bool) {
	e, ok := f.last[id]
	return e.X, e.Y, ok
}
func (f *fakeUI) Last() []UIElement { return f.els }

type fakeInput struct {
	moved, clicked bool
	typed          string
}

func (f *fakeInput) MouseMove(int, int) error       { f.moved = true; return nil }
func (f *fakeInput) MouseClick(string, bool) error  { f.clicked = true; return nil }
func (f *fakeInput) KeyType(s string) error         { f.typed = s; return nil }
func (f *fakeInput) MouseButton(string, bool) error { return nil }
func (f *fakeInput) Scroll(int, int) error          { return nil }
func (f *fakeInput) KeyPress([]string) error        { return nil }

func TestUILayer(t *testing.T) {
	oldUI, oldInput := ui, input
	defer func() { ui, input = oldUI, oldInput }()
	f := &fakeUI{els: []UIElement{
		{ID: 0, Role: "button", Name: "Entregar", X: 100, Y: 200, Enabled: true},
		{ID: 1, Role: "edit", Name: "Nombre", X: 50, Y: 60, Enabled: true},
	}}
	ui = f
	fi := &fakeInput{}
	input = fi
	_, _, _ = f.Snapshot("") // populate last

	ctx := context.Background()

	// ui.find filters by name.
	r, err := (UIFind{}).Invoke(ctx, json.RawMessage(`{"text":"entre"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := r.(map[string]any)["matches"].([]UIElement)
	if len(m) != 1 || m[0].Name != "Entregar" {
		t.Fatalf("find = %+v", m)
	}

	// ui.act by id clicks the element's center.
	if _, err := (UIAct{}).Invoke(ctx, json.RawMessage(`{"id":0}`), nil); err != nil {
		t.Fatal(err)
	}
	if !fi.moved || !fi.clicked {
		t.Fatal("ui.act by id did not move+click")
	}

	// ui.act by name, fill, types the value.
	fi.clicked = false
	if _, err := (UIAct{}).Invoke(ctx, json.RawMessage(`{"name":"Nombre","action":"fill","value":"hola"}`), nil); err != nil {
		t.Fatal(err)
	}
	if fi.typed != "hola" {
		t.Fatalf("fill typed %q", fi.typed)
	}

	// Unknown id is a clear error.
	if _, err := (UIAct{}).Invoke(ctx, json.RawMessage(`{"id":99}`), nil); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

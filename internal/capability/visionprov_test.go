package capability

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionerBuiltinStates(t *testing.T) {
	p := &Provisioner{Mgr: &VisionManager{}, Dir: t.TempDir()}
	state := func() string { return p.Builtin(context.Background()).State }

	if s := state(); s != "off" {
		t.Fatalf("nothing on disk, switched off: got %q, want off", s)
	}
	p.Unsupported = "no NVIDIA GPU"
	if b := p.Builtin(context.Background()); b.State != "unsupported" || b.Reason == "" {
		t.Fatalf("no NVIDIA: got %+v", b)
	}
	p.Unsupported = ""
	if err := os.WriteFile(filepath.Join(p.Dir, "model.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "paused" {
		t.Fatalf("partial model, switched off: got %q, want paused", s)
	}
	p.on = true
	if s := state(); s != "installing" {
		t.Fatalf("switched on, loop not started yet: got %q, want installing", s)
	}
	p.running, p.phase = true, "downloading model"
	if b := p.Builtin(context.Background()); b.State != "installing" || b.Phase != "downloading model" {
		t.Fatalf("provisioning: got %+v", b)
	}
	p.errMsg = "model: connection reset"
	if b := p.Builtin(context.Background()); b.State != "error" || b.Error == "" {
		t.Fatalf("waiting to retry: got %+v", b)
	}
	p.running, p.done, p.errMsg = false, true, ""
	if b := p.Builtin(context.Background()); b.State != "ready" || !b.Required || b.Name != "ui-tars" {
		t.Fatalf("installed: got %+v", b)
	}
}

// Required by default: with no saved choice, Load switches provisioning on,
// unless the PC is unsupported; a saved "off" is respected.
func TestProvisionerLoadDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the loop must not actually download anything
	newProv := func() *Provisioner {
		dir := t.TempDir()
		return &Provisioner{Mgr: &VisionManager{}, Dir: dir, StateFn: filepath.Join(dir, "vision-setup.json"), Default: true, Ctx: ctx}
	}

	p := newProv()
	p.Load()
	if !p.Status()["on"].(bool) {
		t.Fatal("no saved choice: want on by default")
	}
	if b, _ := os.ReadFile(p.StateFn); string(b) != `{"on":true}` {
		t.Fatalf("default not persisted: %q", b)
	}

	p = newProv()
	p.Unsupported = "no NVIDIA GPU"
	p.Load()
	if p.Status()["on"].(bool) {
		t.Fatal("unsupported PC: want off")
	}

	p = newProv()
	if err := os.WriteFile(p.StateFn, []byte(`{"on":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Load()
	if p.Status()["on"].(bool) {
		t.Fatal("saved off: want it respected")
	}
}

// vision.locate must see the endpoint auto-provisioning fills in after the
// capability was registered (it used to keep an empty copy: "Post \"\"").
func TestVisionLocateSeesAutoConfig(t *testing.T) {
	mgr := &VisionManager{}
	r := NewRegistry()
	RegisterVision(r, mgr, nil, nil)
	mgr.autoConfigure(t.TempDir(), "llama-server")
	c, ok := r.Get("vision.locate")
	if !ok {
		t.Fatal("vision.locate not registered")
	}
	if got := c.(VisionLocate).cfg().Endpoint; got != "http://127.0.0.1:8100/v1/chat/completions" {
		t.Fatalf("endpoint = %q", got)
	}
}

// Measured on f3-2 (2560×1440, native shot): the server shrank it to
// 2380×1344 and UI-TARS answered (1026,1318) for the Start button, whose
// centre is at (1102,1416); (1524,745) for a tab centred at (1639,800).
func TestQwenResizeMapsBack(t *testing.T) {
	for _, c := range []struct{ w, h, rw, rh float64 }{
		{2560, 1440, 2380, 1344}, {1568, 882, 1568, 896}, {640, 360, 1204, 672},
	} {
		if rw, rh := qwenResize(c.w, c.h); rw != c.rw || rh != c.rh {
			t.Errorf("qwenResize(%v,%v) = %v×%v, want %v×%v", c.w, c.h, rw, rh, c.rw, c.rh)
		}
	}
	rw, rh := qwenResize(2560, 1440)
	for _, p := range []struct{ mx, my, x, y float64 }{{1026, 1318, 1102, 1416}, {1524, 745, 1639, 800}} {
		x, y := p.mx*2560/rw, p.my*1440/rh
		if math.Abs(x-p.x) > 10 || math.Abs(y-p.y) > 10 {
			t.Errorf("model (%v,%v) → screen (%.0f,%.0f), want ≈(%v,%v)", p.mx, p.my, x, y, p.x, p.y)
		}
	}
}

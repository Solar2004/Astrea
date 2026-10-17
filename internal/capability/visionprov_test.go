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

// Measured on f3-2 (2560×1440 screen, 1568×882 shot): UI-TARS answered y=877
// for a taskbar icon whose centre is at screen y≈1405 (the 882-row image is
// 896 rows for the model). x needs no correction: 1568 is a multiple of 28.
func TestGrid28(t *testing.T) {
	if grid28(882) != 896 || grid28(1568) != 1568 || grid28(5) != 28 {
		t.Fatalf("grid28: %v %v %v", grid28(882), grid28(1568), grid28(5))
	}
	scale := 0.6125
	ih := 1440 * scale
	y := math.Round(877 * ih / grid28(ih) / scale)
	if y < 1395 || y > 1415 {
		t.Fatalf("taskbar y = %v, want ≈1405", y)
	}
}

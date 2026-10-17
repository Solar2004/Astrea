package capability

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionerBuiltinStates(t *testing.T) {
	p := &Provisioner{Mgr: &VisionManager{}, Dir: t.TempDir()}
	state := func() string { return p.Builtin(context.Background()).State }

	if s := state(); s != "missing" {
		t.Fatalf("nothing on disk, switched off: got %q, want missing", s)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, "model.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != "paused" {
		t.Fatalf("partial model, switched off: got %q, want paused", s)
	}
	p.on, p.running, p.phase = true, true, "downloading model"
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

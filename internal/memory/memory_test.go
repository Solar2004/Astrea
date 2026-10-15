package memory

import (
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	s := Open(path)
	a, _ := s.Add("ComfyUI en D:\\ComfyUI", []string{"comfyui"}, "")
	s.Add("otra", nil, "panel")
	s.Add("ComfyUI en D:\\ComfyUI", nil, "") // repetida: se renueva, no se duplica
	if n := len(s.Notes()); n != 2 {
		t.Fatalf("notes = %d, want 2", n)
	}
	if err := s.Forget(a.ID); err == nil {
		t.Fatal("old id of a refreshed note should be gone")
	}
	re := Open(path)
	if len(re.Notes()) != 2 {
		t.Fatal("not persisted")
	}
	n, _ := re.Add("nueva", nil, "")
	if n.ID != "n4" {
		t.Fatalf("id after reopen = %s, want n4", n.ID)
	}
	if err := re.Forget("n4"); err != nil || len(re.Notes()) != 2 {
		t.Fatal("forget")
	}
}

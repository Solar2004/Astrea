package capability

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCleanupOnlyOld(t *testing.T) {
	dir := t.TempDir()
	jobs := filepath.Join(dir, "jobs")
	os.MkdirAll(filepath.Join(jobs, "sub"), 0o755)
	old, fresh := filepath.Join(jobs, "sub", "old.log"), filepath.Join(jobs, "new.log")
	os.WriteFile(old, []byte("12345"), 0o644)
	os.WriteFile(fresh, []byte("1"), 0o644)
	past := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(old, past, past)
	t.Setenv("TMPDIR", filepath.Join(dir, "notatemp")) // not named tmp: must be ignored
	c := &Cleaner{JobsDir: jobs}
	if r := c.Run(context.Background(), 0, true, false); r.Files != 1 || r.Freed != 5 {
		t.Fatalf("dry run = %+v", r)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("dry run deleted")
	}
	c.Run(context.Background(), 0, false, false)
	if _, err := os.Stat(old); err == nil {
		t.Error("old log kept")
	}
	if _, err := os.Stat(filepath.Join(jobs, "sub")); err == nil {
		t.Error("empty folder kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh log deleted")
	}
	if !isTempDir(`C:\Users\x\AppData\Local\Temp`) && runtime.GOOS == "windows" || isTempDir("/home/x") || isTempDir(`C:\Windows`) {
		t.Error("isTempDir")
	}
}

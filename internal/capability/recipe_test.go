package capability

import (
	"archive/zip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Solar2004/Astrea/internal/supervise"
)

func TestSameService(t *testing.T) {
	base := supervise.ServiceSpec{Name: "comfyui", Command: supervise.Command{Command: "python main.py", Cwd: `C:\x`},
		Restart: "on-failure", Autostart: true}
	exposed := base
	exposed.Expose = &supervise.Expose{Endpoint: "comfyui", Port: 8188}
	exposed.Ready = &supervise.Ready{URL: "http://127.0.0.1:8188/"}
	exposed.Command.Env = map[string]string{}
	if !sameService(base, exposed) {
		t.Error("expose/ready/empty env should not count as a change")
	}
	changed := base
	changed.Command.Command = "python main.py --port 9"
	if sameService(base, changed) {
		t.Error("a different command is a change")
	}
}

// Canceling a setup step returns promptly even when the shell left a child
// holding its output pipe.
func TestRunTreeCancelKillsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", "sleep 60 & sleep 60")
	var out strings.Builder
	cmd.Stdout = &out
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	if err := runTree(ctx, cmd); err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestUnzipRefusesEscape(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../evil.txt", "a/../../evil.txt", `..\evil.txt`, "c:x.txt"} {
		zp := filepath.Join(dir, "x.zip")
		f, _ := os.Create(zp)
		zw := zip.NewWriter(f)
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
		zw.Close()
		f.Close()
		out := filepath.Join(dir, "out")
		if err := unzip(zp, out); err == nil {
			t.Errorf("%q: extracted", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err == nil {
			t.Fatalf("%q: wrote outside", name)
		}
	}
	// A normal one still works.
	zp := filepath.Join(dir, "ok.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("sub/ok.txt")
	w.Write([]byte("ok"))
	zw.Close()
	f.Close()
	if err := unzip(zp, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
}

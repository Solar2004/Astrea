package supervise

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipNoSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests use POSIX sh")
	}
}

func TestJobOutcome(t *testing.T) {
	skipNoSh(t)
	m := NewJobs(t.TempDir())
	j, err := m.Start(Command{Command: "echo hola; echo err >&2; exit 3"}, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(context.Background(), j.ID, 5*time.Second)
	if got.State != JobFailed || got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("got %+v", got)
	}
	log, _ := m.Log(j.ID, 0, 0, 0)
	if !strings.Contains(log.Data, "hola") || !strings.Contains(log.Data, "err") {
		t.Fatalf("log missing output: %q", log.Data)
	}

	ok, _ := m.Start(Command{Command: "true"}, "", 0)
	if got, _ := m.Get(context.Background(), ok.ID, 5*time.Second); got.State != JobSucceeded {
		t.Fatalf("got %+v", got)
	}
}

// pidAlive reports whether a pid written by the test command still runs.
func pidAlive(pid string) bool {
	return exec.Command("kill", "-0", pid).Run() == nil
}

func TestJobCancelKillsTree(t *testing.T) {
	skipNoSh(t)
	dir := t.TempDir()
	m := NewJobs(dir)
	pidFile := dir + "/child.pid"
	// The shell spawns a background grandchild; cancel must kill it too.
	j, err := m.Start(Command{Command: fmt.Sprintf("sleep 300 & echo $! > %s; wait", pidFile)}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var pid string
	for i := 0; i < 50 && pid == ""; i++ {
		b, _ := os.ReadFile(pidFile)
		pid = strings.TrimSpace(string(b))
		time.Sleep(20 * time.Millisecond)
	}
	if pid == "" || !pidAlive(pid) {
		t.Fatal("grandchild did not start")
	}
	got, _ := m.Cancel(j.ID)
	if got.State != JobCanceled {
		t.Fatalf("got %+v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if pidAlive(pid) {
		t.Fatal("grandchild survived cancel")
	}
}

func TestJobTimeout(t *testing.T) {
	skipNoSh(t)
	m := NewJobs(t.TempDir())
	j, _ := m.Start(Command{Command: "sleep 30"}, "", 200*time.Millisecond)
	if got, _ := m.Get(context.Background(), j.ID, 10*time.Second); got.State != JobTimedOut {
		t.Fatalf("got %+v", got)
	}
}

func TestServiceRestartAndStop(t *testing.T) {
	skipNoSh(t)
	dir := t.TempDir()
	m := NewServices(dir)
	if _, err := m.Start(ServiceSpec{Name: "crashy", Command: Command{Command: "exit 1"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := m.Status("crashy")
		if st.Restarts >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never restarted: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, err := m.Stop("crashy", false)
	if err != nil || st.State != SvcStopped || st.Autostart {
		t.Fatalf("stop: %+v %v", st, err)
	}

	// Exit 0 under on-failure is final.
	m.Start(ServiceSpec{Name: "once", Command: Command{Command: "true"}})
	time.Sleep(300 * time.Millisecond)
	if st, _ := m.Status("once"); st.State != SvcExited || st.Restarts != 0 {
		t.Fatalf("got %+v", st)
	}

	// Definitions persist; autostart ones come back.
	m.Start(ServiceSpec{Name: "keep", Autostart: true, Command: Command{Command: "sleep 30"}})
	m.StopAll()
	m2 := NewServices(dir)
	if len(m2.List()) != 3 {
		t.Fatalf("persisted %d services", len(m2.List()))
	}
	m2.StartAll()
	defer m2.StopAll()
	if st, _ := m2.Status("keep"); st.State != SvcRunning && st.State != SvcStarting {
		t.Fatalf("autostart not started: %+v", st)
	}
	if st, _ := m2.Status("crashy"); st.State != SvcStopped {
		t.Fatalf("non-autostart started: %+v", st)
	}
}

func TestServiceReadyPort(t *testing.T) {
	skipNoSh(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	m := NewServices(t.TempDir())
	defer m.StopAll()
	m.Start(ServiceSpec{Name: "web", Ready: &Ready{URL: fmt.Sprintf("http://127.0.0.1:%d/", port)},
		Command: Command{Command: fmt.Sprintf("exec python3 -m http.server %d --bind 127.0.0.1", port)}})
	if ok, msg := m.WaitReady(context.Background(), "web", 10*time.Second); !ok {
		t.Fatalf("not ready: %s", msg)
	}
	// A dead service fails fast instead of waiting out the timeout.
	m.Start(ServiceSpec{Name: "dead", Restart: RestartNever, Ready: &Ready{Port: 1}, Command: Command{Command: "exit 2"}})
	start := time.Now()
	if ok, _ := m.WaitReady(context.Background(), "dead", 10*time.Second); ok || time.Since(start) > 5*time.Second {
		t.Fatal("dead service reported ready or took too long")
	}
}

func TestReadLog(t *testing.T) {
	p := t.TempDir() + "/x.log"
	os.WriteFile(p, []byte("uno\ndos\ntres\ncuatro\n"), 0o644)
	c, _ := ReadLog(p, 0, 2, 0)
	if c.Data != "tres\ncuatro\n" || c.NextOffset != c.Size || c.More {
		t.Fatalf("tail: %+v", c)
	}
	c, _ = ReadLog(p, 4, 0, 4)
	if c.Data != "dos\n" || c.NextOffset != 8 || !c.More {
		t.Fatalf("window: %+v", c)
	}
	// Never cut a multi-byte rune.
	os.WriteFile(p, []byte("añ"), 0o644)
	if c, _ = ReadLog(p, 0, 0, 2); c.Data != "a" {
		t.Fatalf("rune cut: %q", c.Data)
	}
	if c, _ := ReadLog(t.TempDir()+"/missing", 0, 5, 0); c.Size != 0 {
		t.Fatal("missing log should be empty")
	}
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", ".hidden", strings.Repeat("a", 65)} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ValidName("llama-qwen_3.5") != nil {
		t.Error("good name rejected")
	}
}

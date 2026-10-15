package update

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ExitRestart is the exit code a process uses to ask its supervisor (systemd
// Restart=always/on-failure) to start it again after an update.
const ExitRestart = 75

// Supervised reports whether systemd runs this process directly (as the main
// process of a unit) and will restart it when it exits with ExitRestart.
// INVOCATION_ID alone is not enough: every program started from a terminal of
// a systemd-managed session inherits it.
func Supervised() bool {
	if v := os.Getenv("ASTREA_SUPERVISED"); v != "" {
		return v != "0"
	}
	if runtime.GOOS != "linux" || os.Getenv("INVOCATION_ID") == "" {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", os.Getppid()))
	return err == nil && strings.TrimSpace(string(comm)) == "systemd"
}

// Respawn starts a fresh copy of exe with args, detached from this process,
// so it outlives it. The caller should exit right after: the new process
// retries its listen address until the old one has released it.
func Respawn(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := startDetached(cmd); err != nil {
		return err
	}
	return cmd.Process.Release()
}

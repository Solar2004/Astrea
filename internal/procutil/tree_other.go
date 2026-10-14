//go:build !windows

package procutil

import (
	"os/exec"
	"syscall"
	"time"
)

// Tree is a started process together with everything it spawns.
type Tree struct{ pid int }

// PrepareTree puts cmd in its own process group so the whole tree can be
// signalled at once. Call before cmd.Start.
func PrepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// AttachTree tracks the tree rooted at the started cmd.
func AttachTree(cmd *exec.Cmd) (*Tree, error) { return &Tree{pid: cmd.Process.Pid}, nil }

// Kill asks the tree to exit (SIGTERM), and SIGKILLs it after grace unless
// done is closed first. done should close when the root process is reaped.
func (t *Tree) Kill(grace time.Duration, done <-chan struct{}) {
	_ = syscall.Kill(-t.pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
	}
	_ = syscall.Kill(-t.pid, syscall.SIGKILL)
}

// Close releases tracking resources.
func (t *Tree) Close() {}

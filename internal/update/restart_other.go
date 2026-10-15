//go:build !windows

package update

import (
	"os/exec"
	"syscall"
)

// startDetached launches cmd in its own session so it survives this process.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

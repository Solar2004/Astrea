//go:build windows

// Package procutil holds platform tweaks for child processes spawned by the agent.
package procutil

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: console children (cmd.exe, nvidia-smi…)
// get no visible console. GUI programs launched through them still show their
// windows, so `start notepad` keeps working.
const createNoWindow = 0x08000000

// NoConsole stops cmd from flashing a console window. The agent is built as a
// GUI-subsystem binary on Windows, so without this every child would open one.
func NoConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

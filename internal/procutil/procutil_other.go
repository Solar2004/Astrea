//go:build !windows

// Package procutil holds platform tweaks for child processes spawned by the agent.
package procutil

import "os/exec"

// NoConsole is a no-op outside Windows.
func NoConsole(*exec.Cmd) {}

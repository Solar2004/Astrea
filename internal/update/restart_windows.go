package update

import (
	"os/exec"
	"syscall"
)

const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

// startDetached launches cmd outside this process's console and, when the
// Task Scheduler job allows it, outside its job object, so ending this task
// run does not take the new agent with it.
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
		HideWindow:    true,
	}
	if err := cmd.Start(); err == nil {
		return nil
	}
	// Breakaway is refused when the job does not permit it; still detach.
	retry := exec.Command(cmd.Path, cmd.Args[1:]...)
	retry.Stdout, retry.Stderr = cmd.Stdout, cmd.Stderr
	retry.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup, HideWindow: true}
	if err := retry.Start(); err != nil {
		return err
	}
	*cmd = *retry
	return nil
}

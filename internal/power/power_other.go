//go:build !windows

package power

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// inhibitor holds a systemd-inhibit child process while sleep is blocked.
type inhibitor struct{ cmd **exec.Cmd }

func newInhibitor() inhibitor { return inhibitor{cmd: new(*exec.Cmd)} }

func (in inhibitor) set(on bool) error {
	if !on {
		if c := *in.cmd; c != nil && c.Process != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
		*in.cmd = nil
		return nil
	}
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return errors.New("systemd-inhibit not found")
	}
	c := exec.Command("systemd-inhibit", "--what=sleep:idle", "--who=astrea", "--why=serving AI workloads", "sleep", "infinity")
	if err := c.Start(); err != nil {
		return err
	}
	*in.cmd = c
	return nil
}

// idleTime uses xprintidle (X11) when available.
func idleTime() (time.Duration, error) {
	out, err := exec.Command("xprintidle").Output()
	if err != nil {
		return 0, errors.New("idle detection needs xprintidle on Linux")
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func foreground() (title, process string) {
	if out, err := exec.Command("xdotool", "getactivewindow", "getwindowname").Output(); err == nil {
		title = strings.TrimSpace(string(out))
	}
	return title, ""
}

// screenLocked is not detected outside Windows yet.
func screenLocked(string) bool { return false }

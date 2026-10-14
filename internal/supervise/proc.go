// Package supervise runs processes that must outlive the tool call that
// started them: one-shot background jobs (pip install, model conversion) and
// long-lived services (llama-server, ComfyUI) with restart policies.
//
// Output goes straight to a log file per process, so nothing is lost while no
// one is watching and the AI can page through it later with an offset.
package supervise

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/shell"
)

// Command is what to run.
type Command struct {
	Command string            `json:"command"`
	Shell   string            `json:"shell,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidName reports whether name is safe to use as an id and a file name.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-64 letters, digits, '.', '_' or '-'", name)
	}
	return nil
}

// maxLog is the size past which a log is rotated to <name>.log.1 on start.
const maxLog = 50 << 20

// process is one running instance of a Command.
type process struct {
	cmd      *exec.Cmd
	tree     *procutil.Tree
	done     chan struct{}
	exitCode int
	err      error
	started  time.Time
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// spawn starts c with stdout and stderr appended to logPath.
func spawn(c Command, logPath string) (*process, error) {
	bin, args, err := shell.Command(c.Shell, c.Command)
	if err != nil {
		return nil, err
	}
	log, err := openLog(logPath)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "\n=== %s start: %s\n", time.Now().Format(time.RFC3339), c.Command)

	cmd := exec.Command(bin, args...)
	cmd.Dir = c.Cwd
	cmd.Env = os.Environ()
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = log, log
	procutil.NoConsole(cmd)
	procutil.PrepareTree(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(log, "=== start failed: %v\n", err)
		log.Close()
		return nil, err
	}
	p := &process{cmd: cmd, done: make(chan struct{}), started: time.Now()}
	if p.tree, err = procutil.AttachTree(cmd); err != nil {
		fmt.Fprintf(log, "=== warning: cannot track child processes (%v); stop may leave orphans\n", err)
	}
	go func() {
		werr := cmd.Wait()
		p.exitCode = cmd.ProcessState.ExitCode()
		var ee *exec.ExitError
		if werr != nil && !errors.As(werr, &ee) {
			p.err = werr
		}
		fmt.Fprintf(log, "=== %s exit code %d\n", time.Now().Format(time.RFC3339), p.exitCode)
		log.Close()
		if p.tree != nil {
			p.tree.Close()
		}
		close(p.done)
	}()
	return p, nil
}

// kill stops the whole process tree and waits for the root to be reaped.
func (p *process) kill() {
	if p.tree != nil {
		p.tree.Kill(5*time.Second, p.done)
	} else {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

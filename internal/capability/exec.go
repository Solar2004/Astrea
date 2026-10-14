package capability

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"

	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/shell"
)

// Exec runs a shell command on the controlled machine, streaming stdout and
// stderr live back to the hub and returning the exit code.
type Exec struct{}

func (Exec) Name() string { return "exec" }

func (Exec) Describe() Descriptor {
	return Descriptor{
		Name:        "exec",
		Description: "Run a shell command on the controlled machine. Streams stdout/stderr live; returns the exit code.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "Command line to run via the system shell."},
				"cwd": {"type": "string", "description": "Working directory. Optional."},
				"shell": {"type": "string", "enum": ["default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"], "description": "Shell or interpreter. Default: cmd on Windows, sh on Linux. Prefer powershell on Windows."},
				"timeout_sec": {"type": "integer", "description": "Kill the command after this many seconds. 0 = no timeout."}
			},
			"required": ["command"]
		}`),
	}
}

type execRequest struct {
	Command    string `json:"command"`
	Cwd        string `json:"cwd"`
	Shell      string `json:"shell"`
	TimeoutSec int    `json:"timeout_sec"`
}

type execResult struct {
	ExitCode int  `json:"exit_code"`
	Killed   bool `json:"killed"`
}

func (Exec) Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error) {
	var req execRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if req.Command == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "command is required"}
	}

	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, durationSec(req.TimeoutSec))
		defer cancel()
	}

	name, args, err := shell.Command(req.Shell, req.Command)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = req.Cwd
	procutil.NoConsole(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, internal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, internal(err)
	}
	if err := cmd.Start(); err != nil {
		return nil, internal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go pump(&wg, stdout, "stdout", emit)
	go pump(&wg, stderr, "stderr", emit)
	wg.Wait()

	err = cmd.Wait()
	res := execResult{}
	if ctx.Err() == context.DeadlineExceeded {
		res.Killed = true
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if err != nil && !res.Killed {
		return nil, internal(err)
	}
	return res, nil
}

// pump reads r line-by-line and emits each chunk on the given channel.
func pump(wg *sync.WaitGroup, r io.Reader, channel string, emit Emit) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		emit(protocol.StreamChunk{Channel: channel, Data: sc.Text() + "\n"})
	}
}

func internal(err error) error {
	return &protocol.Error{Code: protocol.CodeInternal, Message: err.Error()}
}

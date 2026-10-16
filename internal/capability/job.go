package capability

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/supervise"
)

// maxWaitSec bounds how long one call may block waiting on a job or service,
// so it always returns before the hub's default 150s call timeout.
const maxWaitSec = 120

func badRequest(err error) error {
	return &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
}

func badRequestMsg(msg string) error {
	return &protocol.Error{Code: protocol.CodeBadRequest, Message: msg}
}

func decode(payload json.RawMessage, v any) error {
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return badRequest(err)
	}
	return nil
}

func waitDur(sec int) time.Duration { return durationSec(min(max(sec, 0), maxWaitSec)) }

const logArgsSchema = `
				"offset": {"type": "integer", "description": "Byte offset to read from (use next_offset of the previous read to follow the log)."},
				"tail_lines": {"type": "integer", "description": "Return only the last N lines instead (ignores offset)."},
				"max_bytes": {"type": "integer", "description": "Max bytes to return (cap 49152)."}`

type logRequest struct {
	Offset    int64 `json:"offset"`
	TailLines int   `json:"tail_lines"`
	MaxBytes  int   `json:"max_bytes"`
}

// tail is the last lines of a log, attached to status results for context.
func tail(c supervise.LogChunk, err error) string {
	if err != nil {
		return ""
	}
	return c.Data
}

// JobStart launches a command in the background and returns its id at once.
type JobStart struct{ Jobs *supervise.Jobs }

func (JobStart) Name() string { return "job.start" }

func (JobStart) Describe() Descriptor {
	return Descriptor{
		Name: "job.start",
		Description: "Start a long one-shot command in the background (pip install torch, git clone, model conversion…) and return its id immediately. " +
			"Follow it with job.status (wait_sec blocks until it ends) and job.logs. Output goes to a log file, nothing is lost. " +
			"Background children are killed when the job ends: use service.start for servers/daemons.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "Command line to run."},
				"shell": {"type": "string", "enum": ["default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"], "description": "Default: cmd on Windows, sh on Linux. Prefer powershell on Windows."},
				"cwd": {"type": "string"},
				"env": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Extra environment variables."},
				"label": {"type": "string", "description": "Free-form note to recognise the job later."},
				"max_runtime_sec": {"type": "integer", "description": "Kill the job after this long. 0 = no limit."}
			},
			"required": ["command"]
		}`),
	}
}

func (c JobStart) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		supervise.Command
		Label         string `json:"label"`
		MaxRuntimeSec int    `json:"max_runtime_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	j, err := c.Jobs.Start(req.Command, req.Label, durationSec(req.MaxRuntimeSec))
	if err != nil {
		return nil, badRequest(err)
	}
	return j, nil
}

// JobStatus reports one job (optionally waiting for it) or lists all.
type JobStatus struct{ Jobs *supervise.Jobs }

func (JobStatus) Name() string { return "job.status" }

func (JobStatus) Describe() Descriptor {
	return Descriptor{
		Name:        "job.status",
		Description: "Get a background job's state (running|succeeded|failed|canceled|timed_out), exit code and last log lines. Without id, list all jobs.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"wait_sec": {"type": "integer", "description": "Block up to this many seconds (max 120) for the job to finish before answering."}
			}
		}`),
	}
}

func (c JobStatus) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID      string `json:"id"`
		WaitSec int    `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.ID == "" {
		return map[string]any{"jobs": c.Jobs.List()}, nil
	}
	j, err := c.Jobs.Get(ctx, req.ID, waitDur(req.WaitSec))
	if err != nil {
		return nil, badRequest(err)
	}
	return map[string]any{"job": j, "log_tail": tail(c.Jobs.Log(req.ID, 0, 20, 0))}, nil
}

// JobLogs pages through a job's output.
type JobLogs struct{ Jobs *supervise.Jobs }

func (JobLogs) Name() string { return "job.logs" }

func (JobLogs) Describe() Descriptor {
	return Descriptor{
		Name:        "job.logs",
		Description: "Read a background job's combined stdout/stderr log, by byte offset or as the last N lines.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string"},` + logArgsSchema + `
			},
			"required": ["id"]
		}`),
	}
}

func (c JobLogs) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID string `json:"id"`
		logRequest
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	chunk, err := c.Jobs.Log(req.ID, req.Offset, req.TailLines, req.MaxBytes)
	if err != nil {
		return nil, badRequest(err)
	}
	return chunk, nil
}

// JobCancel kills a running job and everything it spawned.
type JobCancel struct{ Jobs *supervise.Jobs }

func (JobCancel) Name() string { return "job.cancel" }

func (JobCancel) Describe() Descriptor {
	return Descriptor{
		Name:        "job.cancel",
		Description: "Cancel a running background job, killing its whole process tree.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"id": {"type": "string"}},
			"required": ["id"]
		}`),
	}
}

func (c JobCancel) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	j, err := c.Jobs.Cancel(req.ID)
	if err != nil {
		return nil, badRequest(err)
	}
	return j, nil
}

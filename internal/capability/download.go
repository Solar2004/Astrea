package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/download"
)

// NetDownload fetches a file onto the controlled machine in the background.
type NetDownload struct{ Mgr *download.Manager }

func (NetDownload) Name() string { return "net.download" }

func (NetDownload) Describe() Descriptor {
	return Descriptor{
		Name: "net.download",
		Description: "Download a file directly on the controlled machine (model weights, installers…), in the background: returns an id at once; " +
			"follow with net.download.status (wait_sec blocks until done). Resumable: an interrupted or canceled download continues from its .part file when started again. " +
			"Checks free disk space first and verifies sha256 if given; skips files already present (and matching sha256). " +
			"url may be hf://<owner>/<repo>/<file>[@<revision>] for HuggingFace (uses HF_TOKEN from the agent's environment for gated repos).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "http(s) URL or hf://owner/repo/path/file.gguf"},
				"dest": {"type": "string", "description": "Target file path, or a directory (existing, or ending in a slash) to keep the server's file name."},
				"sha256": {"type": "string", "description": "Expected SHA-256 (hex). HuggingFace shows it on each file's page."},
				"headers": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Extra request headers, e.g. Authorization."},
				"overwrite": {"type": "boolean", "description": "Download again even if dest exists."},
				"wait_sec": {"type": "integer", "description": "Block up to this many seconds (max 120) for it to finish before answering."}
			},
			"required": ["url", "dest"]
		}`),
	}
}

func (c NetDownload) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		download.Request
		WaitSec int `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	st, err := c.Mgr.Start(req.Request)
	if err != nil {
		return nil, badRequest(err)
	}
	if req.WaitSec > 0 {
		st, _ = c.Mgr.Get(ctx, st.ID, waitDur(req.WaitSec))
	}
	return st, nil
}

// NetDownloadStatus reports one download (optionally waiting) or lists all.
type NetDownloadStatus struct{ Mgr *download.Manager }

func (NetDownloadStatus) Name() string { return "net.download.status" }

func (NetDownloadStatus) Describe() Descriptor {
	return Descriptor{
		Name:        "net.download.status",
		Description: "Progress of a download: state (running|verifying|done|failed|canceled), bytes, total, percent, speed and ETA. Without id, list all downloads.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "string"},
				"wait_sec": {"type": "integer", "description": "Block up to this many seconds (max 120) for it to finish."}
			}
		}`),
	}
}

func (c NetDownloadStatus) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID      string `json:"id"`
		WaitSec int    `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.ID == "" {
		return map[string]any{"downloads": c.Mgr.List()}, nil
	}
	st, err := c.Mgr.Get(ctx, req.ID, waitDur(req.WaitSec))
	if err != nil {
		return nil, badRequest(err)
	}
	return st, nil
}

// NetDownloadCancel stops a download, keeping the partial file for resume.
type NetDownloadCancel struct{ Mgr *download.Manager }

func (NetDownloadCancel) Name() string { return "net.download.cancel" }

func (NetDownloadCancel) Describe() Descriptor {
	return Descriptor{
		Name:        "net.download.cancel",
		Description: "Cancel a running download. The .part file is kept, so net.download with the same url and dest resumes it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"id": {"type": "string"}},
			"required": ["id"]
		}`),
	}
}

func (c NetDownloadCancel) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	st, err := c.Mgr.Cancel(req.ID)
	if err != nil {
		return nil, badRequest(err)
	}
	return st, nil
}

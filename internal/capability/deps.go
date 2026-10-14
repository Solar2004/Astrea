package capability

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Solar2004/Astrea/internal/deps"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// DepsCheck reports which known dependencies (git, python, CUDA, …) are
// installed on the controlled machine, with path and version.
type DepsCheck struct{}

func (DepsCheck) Name() string { return "deps.check" }

func (DepsCheck) Describe() Descriptor {
	return Descriptor{
		Name: "deps.check",
		Description: "Check which dependencies are installed on the controlled machine (path, version, and how the agent would install each). " +
			"Known: " + strings.Join(deps.Names(), ", ") + ". Omit names to check all.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"names": {"type": "array", "items": {"type": "string"}, "description": "Dependencies to check. Omit for all known ones."}
			}
		}`),
	}
}

type depsRequest struct {
	Names      []string `json:"names"`
	TimeoutSec int      `json:"timeout_sec"`
}

func (DepsCheck) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req depsRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}
	list, err := resolveDeps(req.Names)
	if err != nil {
		return nil, err
	}
	// Pick up anything installed since the agent started (by a human or an
	// earlier exec) before reporting it missing.
	deps.RefreshPath()
	out := make([]deps.Status, 0, len(list))
	for _, d := range list {
		out = append(out, deps.Check(ctx, d))
	}
	return map[string]any{"deps": out}, nil
}

// DepsEnsure installs the requested dependencies that are missing, using
// winget on Windows and apt/pacman/dnf on Linux, and verifies each afterwards.
type DepsEnsure struct{}

func (DepsEnsure) Name() string { return "deps.ensure" }

func (DepsEnsure) Describe() Descriptor {
	return Descriptor{
		Name: "deps.ensure",
		Description: "Install the given dependencies if missing (winget on Windows, apt/pacman/dnf on Linux), streaming installer output, then re-check each. " +
			"Already-present ones are left alone. Known: " + strings.Join(deps.Names(), ", ") +
			`; any other package as "winget:<id>", "apt:<pkg>", "pacman:<pkg>" or "dnf:<pkg>". ` +
			"Installs can take minutes (cuda is ~3 GB): always pass timeout_sec, e.g. 900, or 2400 for cuda. " +
			"For CUDA workloads (llama.cpp, PyTorch, ComfyUI) usually only nvidia-driver is required, not the cuda toolkit.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"names": {"type": "array", "items": {"type": "string"}, "description": "Dependencies to ensure, e.g. [\"git\", \"python\", \"uv\"]."},
				"timeout_sec": {"type": "integer", "description": "Abort after this many seconds. Default 1800."}
			},
			"required": ["names"]
		}`),
	}
}

func (DepsEnsure) Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error) {
	var req depsRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if len(req.Names) == 0 {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "names is required"}
	}
	list, err := resolveDeps(req.Names)
	if err != nil {
		return nil, err
	}
	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 1800
	}
	ctx, cancel := context.WithTimeout(ctx, durationSec(req.TimeoutSec))
	defer cancel()

	deps.RefreshPath()
	out := make([]deps.Status, 0, len(list))
	ok := true
	for _, d := range list {
		st := deps.Ensure(ctx, d, func(channel, line string) {
			emit(protocol.StreamChunk{Channel: channel, Data: line})
		})
		if st.Result == "failed" || st.Result == "manual" {
			ok = false
		}
		out = append(out, st)
	}
	return map[string]any{"ok": ok, "deps": out}, nil
}

func resolveDeps(names []string) ([]deps.Dep, error) {
	if len(names) == 0 {
		return deps.Catalog, nil
	}
	out := make([]deps.Dep, 0, len(names))
	for _, n := range names {
		d, err := deps.Lookup(strings.TrimSpace(n))
		if err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
		out = append(out, d)
	}
	return out, nil
}

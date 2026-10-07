package capability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// --- fs.list ---

// FSList lists the entries of a directory on the controlled machine.
type FSList struct{}

func (FSList) Name() string { return "fs.list" }

func (FSList) Describe() Descriptor {
	return Descriptor{
		Name:        "fs.list",
		Description: "List the entries of a directory on the controlled machine.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"path": {"type": "string", "description": "Absolute directory path."}},
			"required": ["path"]
		}`),
	}
}

type fsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
}

func (FSList) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Path == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "path is required"}
	}
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		return nil, internal(err)
	}
	out := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fsEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size(), Mode: info.Mode().String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"path": req.Path, "entries": out}, nil
}

// --- fs.read ---

// FSRead reads a file and returns its contents, base64-encoded for binary safety.
type FSRead struct {
	MaxBytes int64
}

func (FSRead) Name() string { return "fs.read" }

func (FSRead) Describe() Descriptor {
	return Descriptor{
		Name:        "fs.read",
		Description: "Read a file from the controlled machine. Returns base64-encoded bytes.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"path": {"type": "string", "description": "Absolute file path."}},
			"required": ["path"]
		}`),
	}
}

func (f FSRead) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Path == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "path is required"}
	}
	info, err := os.Stat(req.Path)
	if err != nil {
		return nil, internal(err)
	}
	max := f.MaxBytes
	if max == 0 {
		max = 32 << 20 // 32 MiB default guard
	}
	if info.Size() > max {
		return nil, &protocol.Error{Code: protocol.CodeDenied, Message: "file exceeds max read size"}
	}
	data, err := os.ReadFile(req.Path)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{
		"path":   req.Path,
		"size":   info.Size(),
		"base64": base64.StdEncoding.EncodeToString(data),
	}, nil
}

// --- fs.write ---

// FSWrite writes base64-encoded content to a file, creating parent dirs.
type FSWrite struct{}

func (FSWrite) Name() string { return "fs.write" }

func (FSWrite) Describe() Descriptor {
	return Descriptor{
		Name:        "fs.write",
		Description: "Write bytes to a file on the controlled machine, creating parent directories.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute file path."},
				"base64": {"type": "string", "description": "Base64-encoded file contents."},
				"append": {"type": "boolean", "description": "Append instead of truncate."}
			},
			"required": ["path", "base64"]
		}`),
	}
}

func (FSWrite) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Path   string `json:"path"`
		Base64 string `json:"base64"`
		Append bool   `json:"append"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Path == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "path and base64 are required"}
	}
	data, err := base64.StdEncoding.DecodeString(req.Base64)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "invalid base64: " + err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		return nil, internal(err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if req.Append {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(req.Path, flag, 0o644)
	if err != nil {
		return nil, internal(err)
	}
	defer file.Close()
	n, err := file.Write(data)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"path": req.Path, "bytes_written": n}, nil
}

package capability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

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

// FSRead reads a file. Text files come back as plain text; binary files as
// base64.
type FSRead struct {
	MaxBytes int64
}

func (FSRead) Name() string { return "fs.read" }

func (FSRead) Describe() Descriptor {
	return Descriptor{
		Name: "fs.read",
		Description: "Read a file from the controlled machine. UTF-8 text is returned as 'content'; " +
			"binary data as 'base64'. Use offset/limit to page through large files.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute file path."},
				"encoding": {"type": "string", "enum": ["auto", "text", "base64"], "description": "Default auto: text if the bytes are valid UTF-8."},
				"offset": {"type": "integer", "description": "Byte offset to start reading at. Default 0."},
				"limit": {"type": "integer", "description": "Max bytes to return. Default 256 KiB for text, 32 MiB for base64."}
			},
			"required": ["path"]
		}`),
	}
}

func (f FSRead) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
		Offset   int64  `json:"offset"`
		Limit    int64  `json:"limit"`
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
	limit := req.Limit
	if limit <= 0 || limit > max {
		limit = max
		if req.Encoding != "base64" {
			limit = 256 << 10
		}
	}
	file, err := os.Open(req.Path)
	if err != nil {
		return nil, internal(err)
	}
	defer file.Close()
	data := make([]byte, limit)
	n, err := file.ReadAt(data, req.Offset)
	if err != nil && err != io.EOF {
		return nil, internal(err)
	}
	data = data[:n]
	out := map[string]any{
		"path":      req.Path,
		"size":      info.Size(),
		"offset":    req.Offset,
		"bytes":     n,
		"truncated": req.Offset+int64(n) < info.Size(),
	}
	text := req.Encoding == "text" || (req.Encoding != "base64" && utf8.Valid(trimPartialRune(data)))
	if text {
		out["content"] = string(data)
	} else {
		out["base64"] = base64.StdEncoding.EncodeToString(data)
	}
	return out, nil
}

// trimPartialRune drops a UTF-8 sequence cut off by the read limit, so a
// truncated text file is still recognized as text.
func trimPartialRune(b []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		if utf8.RuneStart(b[len(b)-i]) {
			if !utf8.FullRune(b[len(b)-i:]) {
				return b[:len(b)-i]
			}
			break
		}
	}
	return b
}

// --- fs.write ---

// FSWrite writes text or base64-encoded bytes to a file, creating parent dirs.
type FSWrite struct{}

func (FSWrite) Name() string { return "fs.write" }

func (FSWrite) Describe() Descriptor {
	return Descriptor{
		Name:        "fs.write",
		Description: "Write a file on the controlled machine, creating parent directories. Pass text in 'content', or binary data in 'base64'.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute file path."},
				"content": {"type": "string", "description": "Text contents (written as UTF-8)."},
				"base64": {"type": "string", "description": "Base64-encoded contents, for binary files."},
				"append": {"type": "boolean", "description": "Append instead of truncate."}
			},
			"required": ["path"]
		}`),
	}
}

func (FSWrite) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
		Base64  *string `json:"base64"`
		Append  bool    `json:"append"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.Path == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "path is required"}
	}
	var data []byte
	switch {
	case req.Content != nil && req.Base64 != nil:
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "pass content or base64, not both"}
	case req.Content != nil:
		data = []byte(*req.Content)
	case req.Base64 != nil:
		var err error
		if data, err = base64.StdEncoding.DecodeString(*req.Base64); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "invalid base64: " + err.Error()}
		}
	default:
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "content or base64 is required"}
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

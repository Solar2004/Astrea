// Package protocol defines the wire format shared between the Astrea hub
// (the MCP server that Hermes talks to) and the Astrea agent (the daemon on
// the controlled machine).
//
// The control channel is a single WebSocket connection carrying newline-free
// JSON frames. Every request from the hub gets exactly one terminal response
// from the agent (Result or Error), optionally preceded by any number of
// Stream frames carrying incremental output (e.g. live stdout from exec).
package protocol

import (
	"context"
	"encoding/json"
)

// FrameType discriminates the Frame.Type field.
type FrameType string

const (
	// FrameRequest: hub -> agent. Invoke a capability.
	FrameRequest FrameType = "request"
	// FrameStream: agent -> hub. Incremental output for an in-flight request.
	FrameStream FrameType = "stream"
	// FrameResult: agent -> hub. Terminal success for a request.
	FrameResult FrameType = "result"
	// FrameError: agent -> hub. Terminal failure for a request.
	FrameError FrameType = "error"
	// FrameCancel: hub -> agent. Abort the in-flight request with this ID
	// (the hub gave up waiting). The agent still answers with an error frame.
	FrameCancel FrameType = "cancel"
)

// Frame is the single envelope exchanged over the control channel. Exactly one
// of Payload/Result/Error/Stream is meaningful depending on Type.
type Frame struct {
	Type FrameType `json:"type"`
	// ID correlates streams/results/errors back to the originating request.
	ID string `json:"id"`
	// Method is the capability name, e.g. "exec", "fs.read". Set on requests.
	Method string `json:"method,omitempty"`
	// Payload is the method-specific request arguments (requests only).
	Payload json.RawMessage `json:"payload,omitempty"`
	// Result is the method-specific success body (result frames only).
	Result json.RawMessage `json:"result,omitempty"`
	// Stream is an incremental chunk (stream frames only).
	Stream *StreamChunk `json:"stream,omitempty"`
	// Error describes a terminal failure (error frames only).
	Error *Error `json:"error,omitempty"`
	// Work says which piece of declared work a request belongs to (requests
	// from the hub only): "continue" for the machine's current task, or an
	// object {"title", "detail"} that opens a new one. Agents older than
	// v0.4.0 ignore it.
	Work json.RawMessage `json:"work,omitempty"`
}

// StreamChunk is one incremental piece of output for a streaming method.
type StreamChunk struct {
	// Channel is a logical stream name, e.g. "stdout", "stderr".
	Channel string `json:"channel"`
	// Data is the chunk payload. Text methods send UTF-8; binary methods
	// (e.g. screenshots) send base64 and set Encoding="base64".
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

// Error is a structured terminal failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Error codes used across capabilities.
const (
	CodeUnauthorized  = "unauthorized"
	CodeUnknownMethod = "unknown_method"
	CodeBadRequest    = "bad_request"
	CodeInternal      = "internal"
	CodeDenied        = "denied"
	// CodeUnavailable: the capability exists but cannot run right now, e.g.
	// screen/mouse/keyboard while nobody is logged in.
	CodeUnavailable = "unavailable"
)

type ctxKey int

const (
	workSpecKey ctxKey = iota
	workIDKey
)

// WithWorkSpec makes calls under ctx carry spec as their Frame.Work (hub side).
func WithWorkSpec(ctx context.Context, spec json.RawMessage) context.Context {
	return context.WithValue(ctx, workSpecKey, spec)
}

// WorkSpec is the spec set by WithWorkSpec, or nil.
func WorkSpec(ctx context.Context) json.RawMessage {
	s, _ := ctx.Value(workSpecKey).(json.RawMessage)
	return s
}

// WithWorkID records, agent side, the declared task a request runs under.
func WithWorkID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workIDKey, id)
}

// WorkID is the task set by WithWorkID, or "".
func WorkID(ctx context.Context) string {
	s, _ := ctx.Value(workIDKey).(string)
	return s
}

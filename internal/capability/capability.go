// Package capability defines the pluggable actions the agent can perform and
// a registry to dispatch requests to them by method name.
package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// Emit sends an incremental output chunk for a streaming capability. It is safe
// to call zero or many times before returning from Invoke.
type Emit func(protocol.StreamChunk)

// Capability is a single named action the agent exposes.
type Capability interface {
	// Name is the method identifier used on the wire, e.g. "exec".
	Name() string
	// Describe returns a short human description and a JSON schema of the
	// payload, used to advertise the capability as an MCP tool.
	Describe() Descriptor
	// Invoke runs the action. It may stream output via emit and must return a
	// JSON-encodable result or an error. Honor ctx cancellation.
	Invoke(ctx context.Context, payload json.RawMessage, emit Emit) (any, error)
}

// Descriptor is the advertised metadata for a capability.
type Descriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Registry holds the set of capabilities the agent will serve.
type Registry struct {
	mu   sync.RWMutex
	caps map[string]Capability
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{caps: map[string]Capability{}}
}

// Register adds c, replacing any capability with the same name.
func (r *Registry) Register(c Capability) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caps[c.Name()] = c
}

// Get returns the capability for method, if registered.
func (r *Registry) Get(method string) (Capability, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.caps[method]
	return c, ok
}

// Descriptors lists all registered capabilities sorted by name.
func (r *Registry) Descriptors() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.caps))
	for _, c := range r.caps {
		out = append(out, c.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Dispatch invokes the capability named by frame.Method. It is a thin wrapper
// that resolves the method and surfaces an unknown-method error.
func (r *Registry) Dispatch(ctx context.Context, method string, payload json.RawMessage, emit Emit) (any, error) {
	c, ok := r.Get(method)
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeUnknownMethod, Message: fmt.Sprintf("unknown method %q", method)}
	}
	return c.Invoke(ctx, payload, emit)
}

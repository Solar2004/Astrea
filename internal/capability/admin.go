package capability

import (
	"context"
	"encoding/json"
	"runtime"
	"time"

	"github.com/Solar2004/Astrea/internal/security"
)

// AdminLock engages or releases the agent kill switch. While locked, every
// non-admin capability is denied.
type AdminLock struct{ Guard *security.Guard }

func (AdminLock) Name() string { return "admin.lock" }

func (AdminLock) Describe() Descriptor {
	return Descriptor{
		Name:        "admin.lock",
		Description: "Engage (locked=true) or release (locked=false) the kill switch. While locked, only admin.* capabilities run.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"locked": {"type": "boolean"}},
			"required": ["locked"]
		}`),
	}
}

func (a AdminLock) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Locked bool `json:"locked"`
	}
	_ = json.Unmarshal(payload, &req)
	a.Guard.SetLock(req.Locked)
	return map[string]any{"locked": a.Guard.Locked()}, nil
}

// AdminStatus reports agent health, permission mode and lock state.
type AdminStatus struct {
	Guard *security.Guard
	Mode  string
}

func (AdminStatus) Name() string { return "admin.status" }

func (AdminStatus) Describe() Descriptor {
	return Descriptor{
		Name:        "admin.status",
		Description: "Report agent OS, architecture, permission mode (yolo/readonly) and kill-switch (lock) state.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (a AdminStatus) Invoke(_ context.Context, _ json.RawMessage, _ Emit) (any, error) {
	return map[string]any{
		"os":     runtime.GOOS,
		"arch":   runtime.GOARCH,
		"mode":   a.Mode,
		"locked": a.Guard.Locked(),
	}, nil
}

// AdminStop shuts the agent process down gracefully.
type AdminStop struct{ Stop func() }

func (AdminStop) Name() string { return "admin.stop" }

func (AdminStop) Describe() Descriptor {
	return Descriptor{
		Name:        "admin.stop",
		Description: "Shut down the agent process on the controlled machine. It will not restart unless run as a service.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
}

func (a AdminStop) Invoke(_ context.Context, _ json.RawMessage, _ Emit) (any, error) {
	// Trigger shutdown shortly after returning so the result reaches the hub first.
	go func() {
		time.Sleep(250 * time.Millisecond)
		a.Stop()
	}()
	return map[string]any{"stopping": true}, nil
}

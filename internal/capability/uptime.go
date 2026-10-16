package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/uptime"
)

// SysUptime reports when this machine was on, asleep or off, and how busy the
// AI kept it, from the agent's own history (plus the OS log for older days).
type SysUptime struct{ Store *uptime.Store }

func (SysUptime) Name() string { return "sys.uptime" }

func (SysUptime) Describe() Descriptor {
	return Descriptor{
		Name: "sys.uptime",
		Description: "History of when this machine was on: spans (start, end, end_kind running|off|sleep|crash; " +
			"source \"os\" = rebuilt from the OS log, before the agent was installed), time on, current boot, " +
			"and AI actions per hour (actions run under a declared task).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"days": {"type": "integer", "description": "how far back, default 30, max 120"}}
		}`),
	}
}

func (c SysUptime) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Days int `json:"days"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Days <= 0 {
		req.Days = 30
	}
	return c.Store.Report(ctx, min(req.Days, 120)), nil
}

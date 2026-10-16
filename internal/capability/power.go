package capability

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// --- sys.idle ---

// SysIdle reports whether a person is using the machine right now.
type SysIdle struct{}

func (SysIdle) Name() string { return "sys.idle" }

func (SysIdle) Describe() Descriptor {
	return Descriptor{
		Name: "sys.idle",
		Description: "Is someone using the controlled machine? Returns seconds since a person last used the keyboard/mouse " +
			"(Astrea's own input does not count), the focused window and its process (e.g. RobloxPlayerBeta.exe), " +
			"locked (the lock screen is up: nobody is using it), user_active and why. An app left open with nobody " +
			"touching the PC is not active; neither is a single touch, a mouse jiggler, a remote tool or anti-AFK " +
			"software moving the mouse (on Windows the agent reads the raw input: injected input is ignored and " +
			"mouse strokes must look like a hand). Check it before heavy GPU work on a personal PC.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"active_threshold_sec": {"type": "integer", "description": "Input within this many seconds counts as active. Default 120."}}
		}`),
	}
}

func (SysIdle) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	req := struct {
		Threshold int `json:"active_threshold_sec"`
	}{Threshold: 120}
	_ = json.Unmarshal(payload, &req)
	if req.Threshold <= 0 {
		req.Threshold = 120
	}
	idle, err := power.UserIdle(time.Duration(req.Threshold) * time.Second)
	if err != nil {
		return nil, internal(err)
	}
	return idle, nil
}

// --- power.awake ---

// PowerAwake reads or changes the sleep inhibitor mode.
type PowerAwake struct{ Keeper *power.Keeper }

func (PowerAwake) Name() string { return "power.awake" }

func (PowerAwake) Describe() Descriptor {
	return Descriptor{
		Name: "power.awake",
		Description: "Keep the controlled machine from sleeping. mode=auto (default): awake while a service, job or download is running; " +
			"always: never sleep while the agent runs (use it on PCs that must stay reachable: on Modern Standby PCs the screen " +
			"turning off otherwise puts the whole PC to sleep); off: normal power settings. Without mode, reports the current state. " +
			"The mode is saved and survives agent restarts and updates.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"mode": {"type": "string", "enum": ["auto", "always", "off"]}}
		}`),
	}
}

func (p PowerAwake) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(payload, &req)
	if req.Mode != "" {
		if err := p.Keeper.SetMode(req.Mode); err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
		}
	}
	mode, holding := p.Keeper.State()
	return map[string]any{"mode": mode, "sleep_blocked": holding}, nil
}

// --- power.wake ---

// PowerWake sends a Wake-on-LAN packet from this machine.
type PowerWake struct{}

func (PowerWake) Name() string { return "power.wake" }

func (PowerWake) Describe() Descriptor {
	return Descriptor{
		Name: "power.wake",
		Description: "Send a Wake-on-LAN magic packet from this machine to wake another PC on the same local network " +
			"(WoL must be enabled in that PC's BIOS/network adapter). Get the MAC with exec ('getmac' / 'ip link') while it is on.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"mac": {"type": "string", "description": "MAC address, e.g. 3C:7C:3F:12:34:56."},
				"broadcast": {"type": "string", "description": "Broadcast address[:port]. Default 255.255.255.255:9."}
			},
			"required": ["mac"]
		}`),
	}
}

func (PowerWake) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		MAC       string `json:"mac"`
		Broadcast string `json:"broadcast"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.MAC == "" {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "mac is required"}
	}
	if err := power.Wake(req.MAC, req.Broadcast); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	return map[string]any{"sent": true, "mac": req.MAC}, nil
}

// --- power.action ---

// PowerAction locks, sleeps, reboots or shuts the machine down.
type PowerAction struct{}

func (PowerAction) Name() string { return "power.action" }

func (PowerAction) Describe() Descriptor {
	return Descriptor{
		Name: "power.action",
		Description: "Lock the session, sleep, hibernate, reboot or shut down the controlled machine (cancel aborts a pending reboot/shutdown). " +
			"After sleep/shutdown the agent is unreachable until the PC is woken (power.wake from another PC on its LAN).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["lock", "sleep", "hibernate", "reboot", "shutdown", "cancel"]},
				"delay_sec": {"type": "integer", "description": "reboot/shutdown: delay before acting. Default 5."}
			},
			"required": ["action"]
		}`),
	}
}

func (PowerAction) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	req := struct {
		Action string `json:"action"`
		Delay  *int   `json:"delay_sec"`
	}{}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	delay := 5
	if req.Delay != nil && *req.Delay >= 0 {
		delay = *req.Delay
	}
	if _, _, err := power.ActionCommand(req.Action, delay); err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	if req.Action == "sleep" || req.Action == "hibernate" {
		// Answer first: once suspended the result could never be sent.
		go func() {
			time.Sleep(500 * time.Millisecond)
			_, _ = power.Run(req.Action, delay)
		}()
		return map[string]any{"action": req.Action, "scheduled": true}, nil
	}
	out, err := power.Run(req.Action, delay)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"action": req.Action, "delay_sec": delay, "output": out}, nil
}

package capability

import (
	"context"
	"encoding/json"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// This file defines two capabilities for the Windows lock screen on PCs whose
// account has no password (the aula machines):
//
//   - session.nolock keeps the PC from locking in the first place (reversible).
//   - session.unlock signs back in when it is already on the lock screen, by
//     running a helper as SYSTEM on the Winlogon secure desktop.
//
// Both only do something on Windows; elsewhere the backends stay nil and the
// capabilities answer "unavailable". The backends live in session_windows.go.

// nolockBackend turns the auto-lock off (on=true) or restores it (on=false),
// and reports the current state. changed maps each lever to what happened.
type nolockBackend interface {
	Apply(on bool) (changed map[string]string, err error)
	State() (map[string]any, error)
}

// unlockBackend signs in from the lock screen. result may carry an "images"
// entry with a screenshot of the Winlogon desktop.
type unlockBackend interface {
	Unlock(capture bool) (result map[string]any, err error)
}

var (
	nolock nolockBackend
	unlock unlockBackend
)

func unsupported(what string) error {
	return &protocol.Error{Code: protocol.CodeUnavailable, Message: what + " is only available on Windows"}
}

// AutoUnlock signs back in from the lock screen for callers inside the agent
// (the GUI proxies, before an action that needs a visible desktop), rather than
// over the wire. It returns ErrNoUnlock where the OS has no such thing.
func AutoUnlock() (map[string]any, error) {
	if unlock == nil {
		return nil, ErrNoUnlock
	}
	return unlock.Unlock(false)
}

// ErrNoUnlock means this build has no way to drive the lock screen.
var ErrNoUnlock = unsupported("session.unlock")

// --- session.nolock ---

// SessionNoLock stops (or restores) the Windows auto-lock on a passwordless PC.
type SessionNoLock struct{}

func (SessionNoLock) Name() string { return "session.nolock" }

func (SessionNoLock) Describe() Descriptor {
	return Descriptor{
		Name: "session.nolock",
		Description: "Keep this Windows PC from locking by itself (useful on an account with no password, so it never gets " +
			"stuck on the lock screen where Astrea cannot see or control it). on=true disables the lock screen, the " +
			"lock-on-wake and the screensaver password; on=false restores exactly what was there before. Without arguments, " +
			"reports the current state. Reversible. Needs admin (the agent runs elevated).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"on": {"type": "boolean", "description": "true: stop locking. false: restore. Omit: read state."}}
		}`),
	}
}

func (SessionNoLock) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if nolock == nil {
		return nil, unsupported("session.nolock")
	}
	var req struct {
		On *bool `json:"on"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, badRequest(err)
		}
	}
	if req.On == nil {
		st, err := nolock.State()
		if err != nil {
			return nil, internal(err)
		}
		return st, nil
	}
	changed, err := nolock.Apply(*req.On)
	if err != nil {
		return nil, internal(err)
	}
	return map[string]any{"on": *req.On, "changed": changed}, nil
}

// --- session.unlock ---

// SessionUnlock signs back in from the Windows lock screen.
type SessionUnlock struct{}

func (SessionUnlock) Name() string { return "session.unlock" }

func (SessionUnlock) Describe() Descriptor {
	return Descriptor{
		Name: "session.unlock",
		Description: "Sign back in from the Windows lock screen when the account has no password. It launches a helper as " +
			"SYSTEM on the secure Winlogon desktop (where normal input and screen.capture cannot reach) and presses the keys " +
			"to sign in, then checks the desktop went back to normal. Use it when sys.idle reports locked=true. " +
			"With capture=true it also returns a screenshot of the lock screen.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"capture": {"type": "boolean", "description": "Also return a screenshot of the Winlogon desktop."}}
		}`),
	}
}

func (SessionUnlock) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if unlock == nil {
		return nil, unsupported("session.unlock")
	}
	var req struct {
		Capture bool `json:"capture"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, badRequest(err)
		}
	}
	res, err := unlock.Unlock(req.Capture)
	if err != nil {
		return nil, internal(err)
	}
	return res, nil
}

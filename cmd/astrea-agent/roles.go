package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/config"
	"github.com/Solar2004/Astrea/internal/guibridge"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/transport"
)

// Agent roles. On Windows the installer runs two processes of this binary:
//
//   - headless, from a scheduled task at boot as the desktop user (S4U, no
//     session needed): owns the tailnet port, services, jobs, downloads… so
//     models come back after a reboot even if nobody logs in.
//   - gui, from a scheduled task at logon inside the user's desktop: serves
//     only screen/mouse/keyboard/idle on loopback for the headless agent.
//
// full is the classic single process (Linux, or Windows without headless).
const (
	roleFull     = "full"
	roleHeadless = "headless"
	roleGUI      = "gui"
)

// guiCaps are the capabilities that only work inside a desktop session.
func guiCaps() []capability.Capability {
	return []capability.Capability{
		capability.MouseMove{},
		capability.MouseClick{},
		capability.KeyType{},
		capability.MouseButton{},
		capability.MouseScroll{},
		capability.KeyPress{},
		capability.PanelControl{},
		capability.ScreenCapture{},
		capability.SysIdle{},
		capability.ClipboardGet{},
		capability.ClipboardSet{},
		capability.ClipboardHistory{},
		capability.UISnapshot{},
		capability.UIFind{},
		capability.UIAct{},
	}
}

// needsDesktop lists the forwarded capabilities that are useless on a locked
// PC, so the agent signs in first instead of handing back a black screen or a
// "BitBlt failed". sys.idle is deliberately NOT here: it is the read used to
// decide whether the PC is locked, and panel.control is the owner's switch.
func needsDesktop(name string) bool {
	switch {
	case name == "sys.idle", name == "panel.control", strings.HasPrefix(name, "clipboard."):
		return false // the clipboard works on a locked session too
	}
	return true
}

// desktopAction: a call that means someone is working on the desktop, which
// postpones the standby relock (session.lock hold).
func desktopAction(method string) bool {
	switch {
	case strings.HasPrefix(method, "mouse."), strings.HasPrefix(method, "key."),
		strings.HasPrefix(method, "input."), strings.HasPrefix(method, "record."),
		method == "screen.capture", method == "gui.exec", method == "clipboard.set":
		return true
	}
	return false
}

// peekOnly: a screenshot that only looks (the panel's thumbnails).
func peekOnly(args string) bool {
	var p struct {
		Peek bool `json:"peek"`
	}
	_ = json.Unmarshal([]byte(args), &p)
	return p.Peek
}

// unlockGate makes sure the PC is signed in before a desktop action runs, so
// the AI never has to remember to do it: Hermes just asks for a screenshot or
// a click and the lock screen gets out of the way by itself.
type unlockGate struct {
	capability.Capability
	bridge *guibridge.Bridge
	logger *log.Logger
}

// lastAutoUnlock throttles the SYSTEM helper: a burst of clicks must not
// schedule one task per click.
var lastAutoUnlock struct {
	sync.Mutex
	at time.Time
}

func (u unlockGate) Invoke(ctx context.Context, payload json.RawMessage, emit capability.Emit) (any, error) {
	// peek: only look (the panel's thumbnails). Signing in would hide the very
	// thing the owner wants to see: that the PC is locked.
	var p struct {
		Peek bool `json:"peek"`
	}
	_ = json.Unmarshal(payload, &p)
	if !p.Peek {
		u.ensureUnlocked(ctx)
	}
	return u.Capability.Invoke(ctx, payload, emit)
}

// ensureUnlocked asks the helper whether the lock screen is up and, if it is,
// runs the unlock helper. Best effort: any failure just lets the action through
// so the caller gets the real error instead of ours.
func (u unlockGate) ensureUnlocked(ctx context.Context) {
	res, err := u.bridge.Call(ctx, "sys.idle", json.RawMessage(`{}`))
	if err != nil {
		return // nobody logged in, or no helper: nothing we can unlock
	}
	var idle struct {
		Locked bool `json:"locked"`
	}
	if json.Unmarshal(res.Result, &idle) != nil || !idle.Locked {
		return
	}
	lastAutoUnlock.Lock()
	if time.Since(lastAutoUnlock.at) < 30*time.Second {
		lastAutoUnlock.Unlock()
		return
	}
	lastAutoUnlock.at = time.Now()
	lastAutoUnlock.Unlock()

	u.logger.Printf("auto-unlock: the PC is on the lock screen; signing back in before %s", u.Capability.Name())
	out, err := capability.AutoUnlock()
	if err != nil {
		u.logger.Printf("auto-unlock: %v", err)
		return
	}
	u.logger.Printf("auto-unlock: %v", out)
}

// registerGUIProxies advertises the GUI capabilities on the headless agent
// and forwards them to the helper.
func registerGUIProxies(reg *capability.Registry, b *guibridge.Bridge, logger *log.Logger) {
	for _, c := range guiCaps() {
		r := guibridge.Remote{Bridge: b, Local: c}
		if c.Name() == "sys.idle" {
			// No desktop session means nobody can be using the PC: that is an
			// answer, not an error, for "is it free for heavy GPU work?".
			r.Fallback = func() (any, error) {
				return map[string]any{"user_active": false, "session": "none",
					"note": "nobody is logged in on this PC"}, nil
			}
		}
		if needsDesktop(c.Name()) {
			reg.Register(unlockGate{Capability: r, bridge: b, logger: logger})
			continue
		}
		reg.Register(r)
	}
	// exec runs in the headless agent, outside any desktop: GUI programs it
	// starts are invisible. gui.exec runs the command in the user's session.
	guiExec := guibridge.Remote{Bridge: b, Local: capability.Exec{}, As: "gui.exec",
		Description: "Run a command INSIDE the logged-in user's desktop session, so windows it opens are visible " +
			"(e.g. start notepad, open a browser for a screenshot). Same arguments as exec. " +
			"Fails with 'unavailable' if nobody is logged in. Use plain exec for everything else: " +
			"it does not depend on a session, and processes it starts survive logoff."}
	reg.Register(unlockGate{Capability: guiExec, bridge: b, logger: logger})
}

// runGUIHelper serves the GUI capabilities on a loopback address for the
// headless agent. It shares the agent's config (token, allow/deny) but has
// no dynamic endpoints, services or audit log of its own: the headless agent
// already checks and audits every call it forwards.
func runGUIHelper(ctx context.Context, cfg *config.Config, addr string, logger *log.Logger, stop func()) {
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		logger.Fatalf("--gui-addr %q must be a loopback address such as 127.0.0.1:7701: the helper is not meant to be reachable from the network", addr)
	}
	reg := capability.NewRegistry()
	for _, c := range guiCaps() {
		reg.Register(c)
	}
	reg.Register(capability.Exec{})
	reg.Register(capability.DesktopLock{})
	reg.Register(capability.RecordStartRaw{})
	reg.Register(capability.RecordCaptureRaw{})
	reg.Register(capability.RecordingLocal{})
	capability.Clipboard.Start(ctx, filepath.Join(cfg.DataDir, "clipboard-"+sessionUser()+".json"))
	reg.Register(agentRestart{stop: stop})

	guard := security.NewGuard(cfg.EffectiveAllow(), cfg.Deny)
	srv := &transport.Server{Registry: reg, Auth: auth.New(cfg.Token), Guard: guard, Logger: logger}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		ln := listenRetry(ctx, addr, logger)
		if ln == nil {
			return
		}
		logger.Printf("astrea-agent %s (role gui) listening on %s with %d capabilities", version, addr, len(reg.Descriptors()))
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// sessionUser names the clipboard history file: each person's copies are
// their own, even on a PC shared by several accounts.
func sessionUser() string {
	for _, k := range []string{"USERNAME", "USER"} {
		if u := os.Getenv(k); u != "" {
			return filepath.Base(u)
		}
	}
	return "user"
}

// bridgeRecSource forwards recording to the GUI helper, which owns the input
// hooks, and returns the captured events so the main agent can build the task.
type bridgeRecSource struct{ b *guibridge.Bridge }

func (s bridgeRecSource) Start() error {
	_, err := s.b.Call(context.Background(), "record.start", json.RawMessage(`{}`))
	return err
}

func (s bridgeRecSource) StopEvents() ([]capability.RecEvent, error) {
	res, err := s.b.Call(context.Background(), "record.capture", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	var out struct {
		Events []capability.RecEvent `json:"events"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

func (s bridgeRecSource) Recording() (bool, error) {
	res, err := s.b.Call(context.Background(), "record.state", json.RawMessage(`{}`))
	if err != nil {
		if errors.Is(err, guibridge.NoSession) {
			return false, nil
		}
		return false, err
	}
	var out struct {
		Recording bool `json:"recording"`
	}
	_ = json.Unmarshal(res.Result, &out)
	return out.Recording, nil
}

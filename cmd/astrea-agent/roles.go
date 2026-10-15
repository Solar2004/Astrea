package main

import (
	"context"
	"log"
	"net"
	"net/http"
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
	}
}

// registerGUIProxies advertises the GUI capabilities on the headless agent
// and forwards them to the helper.
func registerGUIProxies(reg *capability.Registry, b *guibridge.Bridge) {
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
		reg.Register(r)
	}
	// exec runs in the headless agent, outside any desktop: GUI programs it
	// starts are invisible. gui.exec runs the command in the user's session.
	reg.Register(guibridge.Remote{Bridge: b, Local: capability.Exec{}, As: "gui.exec",
		Description: "Run a command INSIDE the logged-in user's desktop session, so windows it opens are visible " +
			"(e.g. start notepad, open a browser for a screenshot). Same arguments as exec. " +
			"Fails with 'unavailable' if nobody is logged in. Use plain exec for everything else: " +
			"it does not depend on a session, and processes it starts survive logoff."})
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

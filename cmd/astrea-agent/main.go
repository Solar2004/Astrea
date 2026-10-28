// Command astrea-agent is the daemon that runs on the controlled machine
// (Windows or Linux). It serves the Astrea control channel over the tailnet so
// the hub — and through it, Hermes — can drive the machine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/browser"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/config"
	"github.com/Solar2004/Astrea/internal/download"
	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/guibridge"
	"github.com/Solar2004/Astrea/internal/memory"
	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/recipe"
	"github.com/Solar2004/Astrea/internal/schedule"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/supervise"
	"github.com/Solar2004/Astrea/internal/task"
	"github.com/Solar2004/Astrea/internal/transport"
	"github.com/Solar2004/Astrea/internal/update"
	"github.com/Solar2004/Astrea/internal/uptime"
	"github.com/Solar2004/Astrea/internal/work"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// listenRetry binds addr, retrying until it succeeds or ctx ends. At logon
// the agent often starts before Tailscale has assigned the tailnet IP it is
// configured to bind, so a failed bind is expected and transient.
//
// If the address stays taken by another Astrea agent (its /healthz answers)
// for dupAfter, this copy is a duplicate (a second gui helper launched while
// the first was alive) and exits: retrying forever wrote tens of thousands
// of log lines on several PCs. dupAfter is long enough not to trip over the
// hand-over after an update, where the old process still holds the port for
// a few seconds. Failures are logged once, then every 10 minutes.
func listenRetry(ctx context.Context, addr string, logger *log.Logger) net.Listener {
	const dupAfter = 2 * time.Minute
	delay := time.Second
	var takenSince, lastLog time.Time
	fails := 0
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if fails > 0 {
				logger.Printf("listen %s: bound after %d failed tries", addr, fails)
			}
			return ln
		}
		fails++
		if time.Since(lastLog) >= 10*time.Minute {
			logger.Printf("listen %s: %v (try %d, retrying every %s; is Tailscale connected?)", addr, err, fails, delay)
			lastLog = time.Now()
		}
		if agentAnswers(addr) {
			if takenSince.IsZero() {
				takenSince = time.Now()
			} else if time.Since(takenSince) >= dupAfter {
				logger.Printf("listen %s: another Astrea agent has served it for %s; this copy is a duplicate and exits", addr, dupAfter)
				os.Exit(0)
			}
		} else {
			takenSince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay *= 2
		}
	}
}

// agentAnswers reports whether an Astrea agent serves /healthz at addr. A
// wildcard bind (":7700", "0.0.0.0:7700") is asked on loopback: Windows does
// not connect to 0.0.0.0.
func agentAnswers(addr string) bool {
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b := make([]byte, 2)
	n, _ := io.ReadFull(resp.Body, b)
	return resp.StatusCode == http.StatusOK && string(b[:n]) == "ok"
}

// panelPolls are the read-only calls the panel and hub make on their own
// (status, stats, thumbnails…): they say nothing about what the AI did.
var panelPolls = map[string]bool{
	"admin.status": true, "admin.audit": true, "sys.stats": true, "sys.idle": true, "sys.info": true,
	"sys.uptime": true, "service.status": true, "service.logs": true, "work.list": true, "work.get": true,
	"work.shot": true, "memory.get": true, "screen.capture": true, "panel.control": true,
	"capabilities": true, "ping": true, "agent.update": true, "disk.cleanup": true,
}

// openLog opens path for appending, starting fresh once it grows past 5 MiB.
func openLog(path string) (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		flags |= os.O_TRUNC
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, flags, 0o644)
}

func main() {
	// "unlock" is a self-invoked subcommand (a SYSTEM scheduled task launched by
	// the session.unlock capability), not a normal agent start.
	if len(os.Args) > 1 && os.Args[1] == "unlock" {
		os.Exit(runUnlockCmd(os.Args[2:]))
	}
	if logger := run(); logger != nil {
		restartAfterExit(logger)
	}
}

// run is the agent's whole life; it returns the logger once it has shut down
// cleanly (services stopped, logs flushed), nil for one-shot modes.
func run() *log.Logger {
	cfgPath := flag.String("config", config.DefaultPath(), "path to config file")
	addr := flag.String("addr", "", "listen address (overrides config; default 0.0.0.0:7700)")
	token := flag.String("token", "", "bearer token (overrides config / ASTREA_TOKEN)")
	dataDir := flag.String("data-dir", "", "data directory for audit log and endpoint store (overrides config)")
	logFile := flag.String("log-file", "", "also write logs to this file (Windows default: agent.log next to the config)")
	role := flag.String("role", roleFull, "full: everything in one process (default). "+
		"headless: started at boot without a desktop session; GUI capabilities are forwarded to the gui helper. "+
		"gui: helper inside the user's desktop session, serving only GUI capabilities on --gui-addr")
	guiAddr := flag.String("gui-addr", "127.0.0.1:7701", "loopback address where the gui helper listens (roles headless and gui)")
	showVersion := flag.Bool("version", false, "print version and exit")
	diagnose := flag.Bool("diagnose", false, "print a diagnostic report (version, config without token, recent logs) and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("astrea-agent %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	if *diagnose {
		runDiagnose(*cfgPath)
		return nil
	}
	switch *role {
	case roleFull, roleHeadless, roleGUI:
	default:
		log.Fatalf("unknown --role %q (want full, headless or gui)", *role)
	}

	// cleanGUI: a gui helper the headless agent launched for a standard
	// (non-administrator) user. That user cannot read config.json (ACL:
	// Administrators + SYSTEM), so the helper carries its bearer in the
	// environment (ASTREA_GUI_SECRET) and keeps its log/data under %LOCALAPPDATA%.
	cleanGUI := *role == roleGUI && os.Getenv("ASTREA_GUI_SECRET") != ""

	logger := log.New(os.Stderr, "astrea-agent ", log.LstdFlags)
	if *logFile == "" && runtime.GOOS == "windows" {
		// The Windows build has no console, so stderr goes nowhere.
		*logFile = filepath.Join(filepath.Dir(*cfgPath), "agent.log")
		if *role == roleGUI {
			*logFile = filepath.Join(filepath.Dir(*cfgPath), "agent-gui.log")
		}
		if cleanGUI {
			*logFile = filepath.Join(guiDataDir(), "agent-gui.log")
		}
	}
	if *logFile != "" {
		// Not closed on return: the restart after an update logs after run()
		// returns, and the OS closes the file when the process exits.
		if f, err := openLog(*logFile); err == nil {
			// File first: io.MultiWriter stops at the first failing writer, and
			// stderr is an invalid handle in a GUI-subsystem process.
			logger.SetOutput(io.MultiWriter(f, os.Stderr))
		}
	}
	// A crash in a background-launched Windows task is otherwise silent; make
	// sure the panic and stack reach the log the user will send us.
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("FATAL panic: %v\n%s", r, debug.Stack())
			os.Exit(2)
		}
	}()
	logger.Printf("astrea-agent %s %s/%s starting (role %s, config %s)", version, runtime.GOOS, runtime.GOARCH, *role, *cfgPath)
	if exe, err := update.Self(); err == nil {
		update.CleanupOld(exe)
	}

	// Clean mode runs before config.Load: a standard user cannot read
	// config.json, so there is nothing to load — the secret and data dir are
	// supplied here instead.
	if cleanGUI {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		cfg := &config.Config{Token: os.Getenv("ASTREA_GUI_SECRET"), DataDir: guiDataDir()}
		logger.Printf("role gui: clean mode (secret from environment, no config.json), data dir %s", cfg.DataDir)
		runGUIHelper(ctx, cfg, *guiAddr, logger, stop)
		return logger
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatalf("load config %s: %v", *cfgPath, err)
	}
	// Flags and env override file values.
	if *addr != "" {
		cfg.Addr = *addr
	}
	if cfg.Addr == "" {
		cfg.Addr = "0.0.0.0:7700"
	}
	if *token != "" {
		cfg.Token = *token
	} else if env := os.Getenv("ASTREA_TOKEN"); env != "" && cfg.Token == "" {
		cfg.Token = env
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if cfg.DataDir == "" {
		cfg.DataDir = config.DefaultDataDir()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *role == roleGUI {
		runGUIHelper(ctx, cfg, *guiAddr, logger, stop)
		return logger
	}

	// Security: guard (allow/deny + kill switch) and audit log.
	if cfg.Mode == "" {
		cfg.Mode = config.ModeYolo
	}
	guard := security.NewGuard(cfg.EffectiveAllow(), cfg.Deny)
	audit, err := security.NewAudit(filepath.Join(cfg.DataDir, "audit.log"))
	if err != nil {
		logger.Printf("audit log disabled: %v", err)
	}
	defer audit.Close()

	// Uptime: when this PC was on, kept here so the history survives the panel
	// being down; counts the AI's actions (those under a declared task) per hour.
	ups := uptime.Open(filepath.Join(cfg.DataDir, "uptime.json"))
	go ups.Run(ctx)
	// lastAction: the AI's latest action here, for the panel's cards. The
	// panel's own polling and its "from":"panel" input are not the AI.
	var lastAction atomic.Value // security.Entry
	hold := &capability.LockHold{}
	audit.Notify(func(e security.Entry) {
		if desktopAction(e.Method) && !peekOnly(e.Args) {
			hold.Touch() // the AI (or the owner) is working on the desktop: do not relock yet
		}
		if e.Work != "" {
			ups.Count(time.Now())
		}
		if e.Work != "" || (!panelPolls[e.Method] && !strings.Contains(e.Args, `"from":"panel"`)) {
			e.Args = ""
			lastAction.Store(e)
		}
	})

	// Declared work: what the AI says it is doing here, tagged on every action.
	works := work.Open(filepath.Join(cfg.DataDir, "work.json"))
	defer works.Flush()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Second):
				works.Flush()
			}
		}
	}()

	// Dynamic endpoints, persisted so they survive restarts.
	dyn := endpoint.NewManager(filepath.Join(cfg.DataDir, "endpoints.json"))

	reg := capability.NewRegistry()
	// Universal capabilities.
	reg.Register(capability.Exec{})
	reg.Register(capability.FSList{})
	reg.Register(capability.FSRead{})
	reg.Register(capability.FSWrite{})
	reg.Register(capability.SysInfo{})
	reg.Register(&capability.SysStats{})
	reg.Register(capability.ProcList{})
	reg.Register(capability.ProcKill{})
	reg.Register(capability.SysUptime{Store: ups})
	// Microphone: listen to the machine and read/set its input level. The
	// WASAPI backend works at the system level, so it needs no desktop session.
	reg.Register(capability.AudioCapture{})
	reg.Register(capability.MicVolume{})
	// Input, screen and idle detection need the desktop session: served here,
	// or (headless) forwarded to the gui helper running in that session.
	var bridge *guibridge.Bridge
	if *role == roleHeadless {
		bridge = guibridge.New("ws://"+*guiAddr+"/ws", cfg.Token)
		bridge.SignedIn = consoleUser
		registerGUIProxies(reg, bridge, logger)
		go watchGUIHelper(ctx, bridge, cfg.Token, *cfgPath, *guiAddr, logger)
	} else {
		for _, c := range guiCaps() {
			reg.Register(c)
		}
		capability.Clipboard.Start(ctx, filepath.Join(cfg.DataDir, "clipboard-"+sessionUser()+".json"))
	}
	// ui.* lives in the desktop session: in full it is in guiCaps() above; in
	// headless the gui helper serves it and registerGUIProxies forwards it.
	// Dynamic endpoints.
	reg.Register(capability.EndpointCreate{Mgr: dyn})
	reg.Register(capability.EndpointList{Mgr: dyn})
	reg.Register(capability.EndpointDelete{Mgr: dyn})
	// AI-defined tasks: each becomes its own capability (task.<name>). Macro
	// steps go through the same guard and audit log as direct calls.
	// checked runs a capability from inside the agent (macro steps, schedules)
	// through the same guard and audit log as a call from the hub.
	checked := func(ctx context.Context, method string, payload json.RawMessage, emit capability.Emit) (any, error) {
		if err := guard.Check(method); err != nil {
			audit.Log(ctx, method, payload, err)
			return nil, err
		}
		res, err := reg.Dispatch(ctx, method, payload, emit)
		audit.Log(ctx, method, payload, err)
		return res, err
	}
	tasks := task.NewManager(cfg.DataDir)
	tasks.SetDispatch(func(ctx context.Context, method string, payload json.RawMessage, emit task.Emit) (any, error) {
		return checked(ctx, method, payload, capability.Emit(emit))
	})
	capability.RegisterTasks(reg, tasks)
	// Record & replay ("TinyTask"): capture real input, save it as a macro
	// task. The hooks live in the desktop session, so on a headless agent the
	// source forwards to the GUI helper; the task is still saved here.
	recSrc := capability.Local
	if bridge != nil {
		recSrc = bridgeRecSource{b: bridge}
	}
	capability.RegisterRecord(reg, tasks, recSrc)
	dyn.Tasks = func(ctx context.Context, name string, args json.RawMessage, emit func(protocol.StreamChunk)) (any, error) {
		if err := guard.Check("task." + name); err != nil {
			return nil, err
		}
		res, err := tasks.Run(ctx, name, args, emit)
		audit.Log(ctx, "dyn:task."+name, args, err)
		return res, err
	}
	// Dependencies: detect and install git, python, CUDA, ….
	reg.Register(capability.DepsCheck{})
	reg.Register(capability.DepsEnsure{})
	// Background jobs and supervised services (llama-server, ComfyUI, …).
	jobs := supervise.NewJobs(filepath.Join(cfg.DataDir, "jobs"))
	defer jobs.StopAll()
	svcs := supervise.NewServices(filepath.Join(cfg.DataDir, "services"))
	defer svcs.StopAll()
	reg.Register(capability.JobStart{Jobs: jobs})
	reg.Register(capability.JobStatus{Jobs: jobs})
	reg.Register(capability.JobLogs{Jobs: jobs})
	reg.Register(capability.JobCancel{Jobs: jobs})
	reg.Register(capability.ServiceStart{Svcs: svcs, Dyn: dyn})
	reg.Register(capability.ServiceStop{Svcs: svcs, Dyn: dyn})
	// Astrea's own services (vision model…), registered further down.
	var builtins []func(context.Context) capability.Builtin
	reg.Register(capability.ServiceStatus{Svcs: svcs, Builtin: func(ctx context.Context) []capability.Builtin {
		out := make([]capability.Builtin, 0, len(builtins))
		for _, f := range builtins {
			out = append(out, f(ctx))
		}
		return out
	}})
	reg.Register(capability.ServiceLogs{Svcs: svcs})
	go svcs.StartAll()
	downloads := download.NewManager()
	defer downloads.StopAll()
	// Disk: delete old temp files and job logs, on its own past 90%.
	cleaner := &capability.Cleaner{JobsDir: filepath.Join(cfg.DataDir, "jobs"), AutoPercent: 90}
	go cleaner.Loop(ctx)
	reg.Register(capability.DiskCleanup{Cleaner: cleaner})
	reg.Register(capability.NetDownload{Mgr: downloads})
	reg.Register(capability.NetDownloadStatus{Mgr: downloads})
	reg.Register(capability.NetDownloadCancel{Mgr: downloads})
	// Service recipes: install a whole service (artifacts, config, setup,
	// services, endpoints) from a recipe; other machines copy the artifacts
	// from /files/.
	installedFn := filepath.Join(cfg.DataDir, "installed.json")
	recipes, err := recipe.OpenStore(installedFn)
	if err != nil {
		// Keep the broken file for a look and start empty: the next apply
		// adopts what is on disk without downloading it again.
		logger.Printf("recipes: %s unreadable (%v); moved to .bad, starting empty", installedFn, err)
		os.Rename(installedFn, installedFn+".bad")
		if recipes, err = recipe.OpenStore(installedFn); err != nil {
			// Still unreadable (the rename failed: locked on Windows). Run
			// with an empty store that is not saved rather than crash.
			logger.Printf("recipes: %v; recipes are not saved until the agent restarts", err)
			recipes, _ = recipe.OpenStore("")
		}
	}
	installer := &recipe.Installer{
		Env:   &capability.RecipeEnv{Dloads: downloads, Svcs: svcs, Dyn: dyn},
		Store: recipes, Data: cfg.DataDir,
	}
	reg.Register(capability.RecipeApply{In: installer})
	reg.Register(capability.RecipePlan{In: installer})
	reg.Register(capability.RecipeStatus{In: installer})
	reg.Register(capability.RecipeCancel{In: installer})
	reg.Register(capability.RecipeRemove{In: installer})
	if resumed := installer.Resume(); len(resumed) > 0 {
		logger.Printf("recipes: resuming interrupted installs: %v", resumed)
	}
	// Power: keep the PC awake while it serves or downloads (auto), detect a present user.
	// working: a job or download in flight (a restart would lose it).
	working := func() bool {
		for _, j := range jobs.List() {
			if j.State == supervise.JobRunning {
				return true
			}
		}
		for _, d := range downloads.List() {
			if d.State == download.Running || d.State == download.Verifying {
				return true
			}
		}
		return false
	}
	keeper, err := power.NewKeeper(cfg.KeepAwake, func() bool {
		for _, s := range svcs.List() {
			if s.State == "running" || s.State == "starting" || s.State == "backoff" {
				return true
			}
		}
		return working()
	})
	if err != nil {
		logger.Fatalf("keep_awake: %v", err)
	}
	keeper.Restore(filepath.Join(cfg.DataDir, "awake.json")) // a mode set remotely wins over the config
	reg.Register(capability.PowerAwake{Keeper: keeper})
	reg.Register(capability.PowerWake{})
	reg.Register(capability.PowerAction{})
	reg.Register(capability.PowerStandby{})
	// Lock screen (passwordless aula PCs): keep it from locking, and sign back
	// in from it. Both run in this (headless/full) process: it is the elevated
	// desktop user, so it can touch HKLM+HKCU and launch a SYSTEM task; neither
	// needs the GUI helper.
	reg.Register(capability.SessionNoLock{})
	reg.Register(capability.SessionUnlock{})
	// Standby: lock now and keep it locked for passers-by while the AI can
	// still work on it (lockhold.go). Locking must happen in the desktop session.
	hold.Path = filepath.Join(cfg.DataDir, "lockhold.json")
	hold.Logf = logger.Printf
	hold.Lock = func(ctx context.Context) error {
		_, err := capability.DesktopLock{}.Invoke(ctx, nil, nil)
		return err
	}
	hold.Idle = func(context.Context) (power.Idle, error) { return power.UserIdle(2 * time.Minute) }
	if bridge != nil {
		hold.Lock = func(ctx context.Context) error {
			_, err := bridge.Call(ctx, "session.lock", json.RawMessage(`{}`))
			if errors.Is(err, guibridge.NoSession) {
				return nil // nobody signed in: it is already at the sign-in screen
			}
			return err
		}
		hold.Idle = func(ctx context.Context) (power.Idle, error) {
			var idle power.Idle
			res, err := bridge.Call(ctx, "sys.idle", json.RawMessage(`{}`))
			if err == nil {
				err = json.Unmarshal(res.Result, &idle)
			}
			return idle, err
		}
	}
	reg.Register(capability.SessionLock{Hold: hold})
	go hold.Run(ctx)
	// Browser: a real Chromium Astrea drives by CDP (navigate, read as text, click, fill, JS).
	br := &browser.Browser{DataDir: cfg.DataDir, Headless: cfg.BrowserHeadless}
	defer br.Close()
	capability.RegisterBrowser(reg, br)
	// Fine-grained control layer 2 (UI Automation) and layer 3 (local vision
	// model). ui.* runs in the desktop session (gui caps / forwarded). vision
	// runs here and dispatches screenshot + click through the guard.
	visionMgr := capability.NewVisionManager(capability.VisionConfig{
		Endpoint: cfg.Vision.Endpoint, Model: cfg.Vision.Model, APIKey: cfg.Vision.APIKey,
		Serve: cfg.Vision.Serve, IdleStopSec: cfg.Vision.IdleStopSec, Coords: cfg.Vision.Coords,
	}, logger.Printf)
	visionProv := &capability.Provisioner{
		Mgr: visionMgr, Dir: filepath.Join(cfg.DataDir, "vision"), Dloads: downloads,
		StateFn: filepath.Join(cfg.DataDir, "vision-setup.json"), Logf: logger.Printf,
		ModelURL: cfg.Vision.ModelURL, MMProjURL: cfg.Vision.MMProjURL, Ctx: ctx,
	}
	capability.RegisterVision(reg, visionMgr, visionProv, func(ctx context.Context, method string, args json.RawMessage) (any, error) {
		return checked(ctx, method, args, func(protocol.StreamChunk) {})
	})
	builtins = append(builtins, visionProv.Builtin)
	go visionMgr.Loop(ctx) // auto-stop the model when idle; starts nothing by itself
	// Required on every PC: provisioning starts by itself unless switched off
	// with vision.setup or this PC cannot run it.
	visionProv.Default = true
	visionProv.Unsupported = capability.VisionUnsupported(ctx)
	visionProv.Load()
	if cfg.Vision.AutoProvision {
		_ = visionProv.SetOn(true) // config forces it on, even over a saved "off"
	}
	// Scheduled calls ("lock at 08:00", "run task.x every 30m"), in the PC's local time.
	sched := schedule.Open(filepath.Join(cfg.DataDir, "schedule.json"), func(ctx context.Context, method string, args json.RawMessage) (any, error) {
		return checked(ctx, method, args, func(protocol.StreamChunk) {})
	}, logger.Printf)
	capability.RegisterSchedule(reg, sched)
	go sched.Run(ctx)
	// Self-update from GitHub Releases (agent.update) and restart.
	upd := &updater{mode: orDefault(cfg.AutoUpdate, "auto"), stop: stop, logger: logger, busy: working}
	if bridge != nil {
		upd.gui = func(ctx context.Context) error {
			if !bridge.Available(ctx) {
				return nil // nobody logged in: the helper starts with the new binary
			}
			_, err := bridge.Call(ctx, "agent.restart", json.RawMessage(`{}`))
			return err
		}
	}
	reg.Register(agentUpdate{u: upd})
	reg.Register(agentRestart{stop: stop})
	go upd.loop(ctx)
	// Admin / kill switch.
	reg.Register(capability.AdminLock{Guard: guard})
	capability.RegisterWork(reg, works, audit.Path(), filepath.Join(cfg.DataDir, "shots"))
	// Memory: what this PC has and what was done here, handed to the AI when it starts a task.
	capability.RegisterMemory(reg, &capability.Memory{Notes: memory.Open(filepath.Join(cfg.DataDir, "memory.json")), Works: works, Reg: reg})
	reg.Register(capability.AdminStatus{Guard: guard, Mode: cfg.Mode, Extra: func() map[string]any {
		m := upd.status()
		if c := cleaner.Last(); c != nil {
			m["cleanup"] = c
		}
		if e, ok := lastAction.Load().(security.Entry); ok {
			m["last_action"] = e
		}
		if t, ok := works.Current(); ok {
			m["work"] = t
		}
		return m
	}})
	reg.Register(capability.AdminStop{Stop: stop})
	reg.Register(capability.AdminAudit{Path: audit.Path()})

	tok := auth.New(cfg.Token)
	if !tok.Enabled() {
		logger.Println("WARNING: no token set — control channel is UNAUTHENTICATED. Set a token in config or --token.")
	}

	srv := &transport.Server{Registry: reg, Auth: tok, Guard: guard, Audit: audit, Work: works, Dyn: dyn, Files: &recipe.Files{Store: recipes, Data: cfg.DataDir}, Logger: logger}
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}

	go func() {
		ln := listenRetry(ctx, cfg.Addr, logger)
		if ln == nil {
			return
		}
		logger.Printf("astrea-agent %s (role %s) listening on %s with %d capabilities, mode %s (data dir %s)", version, *role, cfg.Addr, len(reg.Descriptors()), cfg.Mode, cfg.DataDir)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Println("shutting down")
	// After an update, never stay down because stopping something hangs
	// (a service that ignores the stop, a stuck job): restart regardless.
	if restartRequested.Load() {
		time.AfterFunc(20*time.Second, func() {
			logger.Println("shutdown took more than 20s; restarting anyway")
			if restartAfterExit(logger) {
				os.Exit(0)
			}
		})
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return logger
}

// Command astrea-agent is the daemon that runs on the controlled machine
// (Windows or Linux). It serves the Astrea control channel over the tailnet so
// the hub — and through it, Hermes — can drive the machine.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/config"
	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/power"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/supervise"
	"github.com/Solar2004/Astrea/internal/task"
	"github.com/Solar2004/Astrea/internal/transport"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// listenRetry binds addr, retrying until it succeeds or ctx ends. At logon
// the agent often starts before Tailscale has assigned the tailnet IP it is
// configured to bind, so a failed bind is expected and transient.
func listenRetry(ctx context.Context, addr string, logger *log.Logger) net.Listener {
	delay := time.Second
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln
		}
		logger.Printf("listen %s: %v (retrying in %s; is Tailscale connected?)", addr, err, delay)
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
	cfgPath := flag.String("config", config.DefaultPath(), "path to config file")
	addr := flag.String("addr", "", "listen address (overrides config; default 0.0.0.0:7700)")
	token := flag.String("token", "", "bearer token (overrides config / ASTREA_TOKEN)")
	dataDir := flag.String("data-dir", "", "data directory for audit log and endpoint store (overrides config)")
	logFile := flag.String("log-file", "", "also write logs to this file (Windows default: agent.log next to the config)")
	flag.Parse()

	logger := log.New(os.Stderr, "astrea-agent ", log.LstdFlags)
	if *logFile == "" && runtime.GOOS == "windows" {
		// The Windows build has no console, so stderr goes nowhere.
		*logFile = filepath.Join(filepath.Dir(*cfgPath), "agent.log")
	}
	if *logFile != "" {
		if f, err := openLog(*logFile); err == nil {
			defer f.Close()
			// File first: io.MultiWriter stops at the first failing writer, and
			// stderr is an invalid handle in a GUI-subsystem process.
			logger.SetOutput(io.MultiWriter(f, os.Stderr))
		}
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
	// Input and screen.
	reg.Register(capability.MouseMove{})
	reg.Register(capability.MouseClick{})
	reg.Register(capability.KeyType{})
	reg.Register(capability.ScreenCapture{})
	// Dynamic endpoints.
	reg.Register(capability.EndpointCreate{Mgr: dyn})
	reg.Register(capability.EndpointList{Mgr: dyn})
	reg.Register(capability.EndpointDelete{Mgr: dyn})
	// AI-defined tasks: each becomes its own capability (task.<name>). Macro
	// steps go through the same guard and audit log as direct calls.
	tasks := task.NewManager(cfg.DataDir)
	tasks.SetDispatch(func(ctx context.Context, method string, payload json.RawMessage, emit task.Emit) (any, error) {
		if err := guard.Check(method); err != nil {
			audit.Log(method, payload, err)
			return nil, err
		}
		res, err := reg.Dispatch(ctx, method, payload, capability.Emit(emit))
		audit.Log(method, payload, err)
		return res, err
	})
	capability.RegisterTasks(reg, tasks)
	dyn.Tasks = func(ctx context.Context, name string, args json.RawMessage, emit func(protocol.StreamChunk)) (any, error) {
		if err := guard.Check("task." + name); err != nil {
			return nil, err
		}
		res, err := tasks.Run(ctx, name, args, emit)
		audit.Log("dyn:task."+name, args, err)
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
	reg.Register(capability.ServiceStatus{Svcs: svcs})
	reg.Register(capability.ServiceLogs{Svcs: svcs})
	go svcs.StartAll()
	// Power: keep the PC awake while it serves (auto), detect a present user.
	keeper, err := power.NewKeeper(cfg.KeepAwake, func() bool {
		for _, s := range svcs.List() {
			if s.State == "running" || s.State == "starting" || s.State == "backoff" {
				return true
			}
		}
		for _, j := range jobs.List() {
			if j.State == supervise.JobRunning {
				return true
			}
		}
		return false
	})
	if err != nil {
		logger.Fatalf("keep_awake: %v", err)
	}
	reg.Register(capability.SysIdle{})
	reg.Register(capability.PowerAwake{Keeper: keeper})
	reg.Register(capability.PowerWake{})
	reg.Register(capability.PowerAction{})
	// Admin / kill switch.
	reg.Register(capability.AdminLock{Guard: guard})
	reg.Register(capability.AdminStatus{Guard: guard, Mode: cfg.Mode})
	reg.Register(capability.AdminStop{Stop: stop})

	tok := auth.New(cfg.Token)
	if !tok.Enabled() {
		logger.Println("WARNING: no token set — control channel is UNAUTHENTICATED. Set a token in config or --token.")
	}

	srv := &transport.Server{Registry: reg, Auth: tok, Guard: guard, Audit: audit, Dyn: dyn, Logger: logger}
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}

	go func() {
		ln := listenRetry(ctx, cfg.Addr, logger)
		if ln == nil {
			return
		}
		logger.Printf("astrea-agent %s listening on %s with %d capabilities, mode %s (data dir %s)", version, cfg.Addr, len(reg.Descriptors()), cfg.Mode, cfg.DataDir)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// Command astrea-agent is the daemon that runs on the controlled machine
// (Windows or Linux). It serves the Astrea control channel over the tailnet so
// the hub — and through it, Hermes — can drive the machine.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/config"
	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/Solar2004/Astrea/internal/transport"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", config.DefaultPath(), "path to config file")
	addr := flag.String("addr", "", "listen address (overrides config; default 0.0.0.0:7700)")
	token := flag.String("token", "", "bearer token (overrides config / ASTREA_TOKEN)")
	dataDir := flag.String("data-dir", "", "data directory for audit log and endpoint store (overrides config)")
	flag.Parse()

	logger := log.New(os.Stderr, "astrea-agent ", log.LstdFlags)

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
	guard := security.NewGuard(cfg.Allow, cfg.Deny)
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
	// Admin / kill switch.
	reg.Register(capability.AdminLock{Guard: guard})
	reg.Register(capability.AdminStatus{Guard: guard})
	reg.Register(capability.AdminStop{Stop: stop})

	tok := auth.New(cfg.Token)
	if !tok.Enabled() {
		logger.Println("WARNING: no token set — control channel is UNAUTHENTICATED. Set a token in config or --token.")
	}

	srv := &transport.Server{Registry: reg, Auth: tok, Guard: guard, Audit: audit, Dyn: dyn, Logger: logger}
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}

	go func() {
		logger.Printf("astrea-agent %s listening on %s with %d capabilities (data dir %s)", version, cfg.Addr, len(reg.Descriptors()), cfg.DataDir)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

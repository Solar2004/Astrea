package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/Solar2004/Astrea/internal/update"
)

// runUpdate is `astrea-hub update`: brings this machine's hub and panel to
// the latest release and asks every agent of the fleet to do the same.
//
//	astrea-hub update            # hub + panel + all agents
//	astrea-hub update --check    # only report versions
//	astrea-hub update --local    # hub + panel only
//	astrea-hub update --fleet    # agents only
func runUpdate(args []string) {
	fs := flag.NewFlagSet("astrea-hub update", flag.ExitOnError)
	cfgPath := fs.String("config", fleet.DefaultPath(), "machines file")
	check := fs.Bool("check", false, "only report current and latest versions")
	local := fs.Bool("local", false, "only update hub and panel on this machine")
	onlyFleet := fs.Bool("fleet", false, "only update the agents")
	force := fs.Bool("force", false, "reinstall even if up to date; update agents even with jobs running")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	report := map[string]any{}
	failed := false

	latest, err := update.Latest(ctx, update.Base())
	if err != nil {
		fmt.Fprintln(os.Stderr, "no se pudo consultar la última versión:", err)
		os.Exit(1)
	}
	report["latest"] = latest
	say := func(format string, a ...any) {
		if !*asJSON {
			fmt.Printf(format+"\n", a...)
		}
	}
	say("Última versión de Astrea: %s", latest)

	if !*onlyFleet {
		res := updateLocal(ctx, latest, *check, *force, say)
		for _, r := range res {
			failed = failed || r.Error != ""
		}
		report["local"] = res
	}

	if !*local {
		machines, err := fleet.Load(*cfgPath)
		if err != nil {
			say("flota: %v", err)
		} else {
			res := fleet.New(machines).UpdateAll(ctx, *check, *force)
			for _, r := range res {
				switch {
				case r.Error != "":
					failed = true
					say("  ✗ %-18s %s", r.Name, r.Error)
				case r.Updated:
					say("  ↑ %-18s %s → %s (reiniciando)", r.Name, r.Current, r.Target)
				case *check && update.Newer(r.Current, r.Latest):
					say("  • %-18s %s (hay %s)", r.Name, r.Current, r.Latest)
				default:
					say("  ✓ %-18s %s al día", r.Name, r.Current)
				}
			}
			report["fleet"] = res
		}
	}

	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(report)
	}
	if failed {
		os.Exit(1)
	}
}

// localResult is update.Result plus an error message for the report.
type localResult struct {
	update.Result
	Error string `json:"error,omitempty"`
}

// updateLocal updates this hub binary and the panel installed next to it,
// then restarts the panel's systemd user service so it runs the new one.
// The hub itself is a short-lived MCP process: Hermes starts the new binary
// the next time it loads the server.
func updateLocal(ctx context.Context, latest string, check, force bool, say func(string, ...any)) []localResult {
	self, err := update.Self()
	if err != nil {
		return []localResult{{Result: update.Result{Component: "astrea-hub"}, Error: err.Error()}}
	}
	dir := filepath.Dir(self)
	panel := filepath.Join(dir, "astrea-panel")
	if runtime.GOOS == "windows" {
		panel += ".exe"
	}

	items := []struct{ component, path, current string }{{"astrea-hub", self, version}}
	if _, err := os.Stat(panel); err == nil {
		items = append(items, struct{ component, path, current string }{"astrea-panel", panel, binaryVersion(panel)})
	}

	var out []localResult
	panelUpdated := false
	for _, it := range items {
		r := localResult{Result: update.Result{Component: it.component, From: it.current, To: latest, Path: it.path}}
		switch {
		case check:
			if outdated(it.current, latest) {
				say("  • %-18s %s (hay %s)", it.component, it.current, latest)
			} else {
				say("  ✓ %-18s %s al día", it.component, orDev(it.current))
			}
		case !force && !outdated(it.current, latest):
			say("  ✓ %-18s %s al día", it.component, orDev(it.current))
		default:
			if err := update.Apply(ctx, update.Base(), it.component, latest, it.path, nil); err != nil {
				r.Error = err.Error()
				say("  ✗ %-18s %v", it.component, err)
			} else {
				r.Updated = true
				say("  ↑ %-18s %s → %s", it.component, orDev(it.current), latest)
				panelUpdated = panelUpdated || it.component == "astrea-panel"
			}
		}
		out = append(out, r)
	}
	if panelUpdated && runtime.GOOS == "linux" {
		if err := exec.Command("systemctl", "--user", "try-restart", "astrea-panel").Run(); err == nil {
			say("  panel reiniciado (systemctl --user)")
		}
	}
	return out
}

// binaryVersion asks a binary for its version ("astrea-panel v0.3.0 …").
// Binaries from before --version existed report "".
func binaryVersion(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return ""
	}
	var name, v string
	_, _ = fmt.Sscan(string(out), &name, &v)
	return v
}

// outdated: a binary with no version predates --version (< v0.3.0), so it
// is older than any release; "dev" builds are left alone.
func outdated(current, latest string) bool {
	return current == "" || update.Newer(current, latest)
}

func orDev(v string) string {
	if v == "" {
		return "(sin versión)"
	}
	return v
}

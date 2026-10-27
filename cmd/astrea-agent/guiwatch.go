package main

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/Solar2004/Astrea/internal/guibridge"
)

// guiTask is the scheduled task that starts the gui helper at logon
// (scripts/install.ps1).
const guiTask = "AstreaAgentGUI"

// watchGUIHelper brings the gui helper back when someone is signed in but it
// does not answer. It only starts at logon and restarts itself after an
// update; if that respawn fails (Smart App Control blocking a just-replaced
// binary, a crash) screen, mouse and keyboard stay dead until the next logon.
//
// Relaunching the task is not enough when the helper is alive but hung:
// schtasks /run does nothing while the task still counts as running. That
// left the helper dead for hours on several PCs (2026-10-23). So if a
// relaunch did not bring it back, the next attempt first kills the helper
// processes in the console session.
//
// It first makes sure the task starts for every administrator, not only the
// account that installed Astrea (migrateGUITask).
//
// Who the console user is decides how the helper is brought back (launchHelper,
// Windows): an administrator gets the logon task (reads config.json, cfg.Token);
// a standard account — a pupil — gets a helper the headless agent launches
// itself in that user's session, as that user but with administrator rights
// injected into the process token, authenticated with a per-launch secret. The
// bridge token follows whichever helper is up (b.SetToken).
func watchGUIHelper(ctx context.Context, b *guibridge.Bridge, cfgToken, cfgPath, guiAddr string, logger *log.Logger) {
	if changed, err := migrateGUITask(cfgPath, guiAddr); err != nil {
		logger.Printf("gui helper: could not open task %s to every administrator: %v", guiTask, err)
	} else if changed {
		logger.Printf("gui helper: task %s now starts for any administrator who signs in", guiTask)
	}
	const every = 30 * time.Second
	misses := 0
	relaunches := 0         // since the helper last answered
	wait := 2 * time.Minute // between relaunches; doubles while they fail
	var last time.Time
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := b.Probe(ctx)
		if err == nil {
			if relaunches > 0 {
				logger.Printf("gui helper: answering again after %d relaunch(es)", relaunches)
			}
			misses, relaunches, wait = 0, 0, 2*time.Minute
			continue
		}
		user := consoleUser()
		if user == "" || restartRequested.Load() {
			misses = 0 // nobody signed in, or we are restarting ourselves
			continue
		}
		// Two misses in a row: a helper that is just restarting is back in ~1 s.
		if misses++; misses < 2 || time.Since(last) < wait {
			continue
		}
		last = time.Now()
		killed := ""
		if relaunches > 0 {
			if n, kerr := killSessionHelpers(); kerr != nil {
				killed = "; killing the hung helper failed: " + kerr.Error()
			} else if n > 0 {
				killed = "; killed " + strconv.Itoa(n) + " hung helper process(es) first"
			}
		}
		relaunches++
		desc, rerr := launchHelper(b, cfgToken, guiAddr, logger)
		if rerr != nil {
			logger.Printf("gui helper: %s is signed in but the helper does not answer (%v)%s; relaunch failed: %v (retry in %s)", user, err, killed, rerr, wait)
		} else {
			logger.Printf("gui helper: %s is signed in but the helper does not answer (%v)%s; relaunched %s", user, err, killed, desc)
		}
		if wait < 30*time.Minute {
			wait *= 2
		}
	}
}

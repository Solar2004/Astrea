package main

import (
	"context"
	"log"
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
func watchGUIHelper(ctx context.Context, b *guibridge.Bridge, logger *log.Logger) {
	const every = 30 * time.Second
	misses := 0
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
		if b.Available(ctx) {
			misses, wait = 0, 2*time.Minute
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
		if err := startGUITask(); err != nil {
			logger.Printf("gui helper: %s is signed in but the helper is down; relaunch failed: %v (retry in %s)", user, err, wait)
		} else {
			logger.Printf("gui helper: %s is signed in but the helper was down; relaunched task %s", user, guiTask)
		}
		if wait < 30*time.Minute {
			wait *= 2
		}
	}
}

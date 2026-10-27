//go:build !windows

package main

import (
	"errors"
	"log"

	"github.com/Solar2004/Astrea/internal/guibridge"
)

// consoleUser is Windows-only: elsewhere the helper is the desktop's job, so
// the watchdog never fires.
func consoleUser() string { return "" }

func startGUITask() error { return errors.New("only on Windows") }

func killSessionHelpers() (int, error) { return 0, nil }

func migrateGUITask(cfgPath, guiAddr string) (bool, error) { return false, nil }

func offConsole() bool { return false }

func launchHelper(_ *guibridge.Bridge, _, _ string, _ *log.Logger) (string, error) {
	return "", errors.New("only on Windows")
}

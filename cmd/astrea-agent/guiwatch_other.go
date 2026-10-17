//go:build !windows

package main

import "errors"

// consoleUser is Windows-only: elsewhere the helper is the desktop's job, so
// the watchdog never fires.
func consoleUser() string { return "" }

func startGUITask() error { return errors.New("only on Windows") }

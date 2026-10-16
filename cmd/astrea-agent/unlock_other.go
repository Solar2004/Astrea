//go:build !windows

package main

import (
	"fmt"
	"os"
)

// runUnlockCmd exists only so main can dispatch the "unlock" subcommand on any
// OS; the lock screen it drives is Windows-only.
func runUnlockCmd(_ []string) int {
	fmt.Fprintln(os.Stderr, "astrea-agent unlock is only available on Windows")
	return 2
}

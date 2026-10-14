//go:build !windows

package deps

import "os/exec"

func lookPath(name string) (string, error) { return exec.LookPath(name) }

// RefreshPath is a no-op outside Windows: Linux package managers install into
// directories already on PATH.
func RefreshPath() {}

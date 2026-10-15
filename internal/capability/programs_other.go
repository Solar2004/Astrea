//go:build !windows

package capability

// installedPrograms: only Windows keeps a list worth reporting; on Linux the
// tools (deps) say what matters.
func installedPrograms() []string { return nil }

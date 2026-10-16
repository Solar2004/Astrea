//go:build !windows

package power

// startHooks: no input hooks outside Windows; the idle timer is used instead.
func startHooks(*tracker) bool { return false }

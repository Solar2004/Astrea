//go:build !windows

package capability

// Outside Windows there is no lock screen to manage: the backends stay nil and
// session.nolock / session.unlock answer "unavailable".

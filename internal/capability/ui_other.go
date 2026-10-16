//go:build !windows

package capability

// No UI Automation outside Windows: ui.* answers "unavailable" (requireUI).
// A Linux port would read the AT-SPI accessibility tree.

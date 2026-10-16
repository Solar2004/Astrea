//go:build !windows

package capability

// No input recorder outside Windows yet: the capabilities answer "unavailable"
// (requireRecorder). Linux would need an XRecord / libinput reader.

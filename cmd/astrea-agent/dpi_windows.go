//go:build windows

package main

import "golang.org/x/sys/windows"

// init makes the agent DPI-aware before anything touches the screen. Without
// it, Windows with display scaling (125 %, 150 %…) reports logical sizes: a
// 1920×1080 monitor at 150 % looks like 1280×720, so screen.capture grabbed only
// the top-left part of the screen and mouse.move coordinates didn't match the
// screenshot. Per-monitor v2 keeps capture and input in physical pixels.
func init() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	// Windows 10 1703+: DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == (HANDLE)-4.
	if p := user32.NewProc("SetProcessDpiAwarenessContext"); p.Find() == nil {
		if ok, _, _ := p.Call(^uintptr(3)); ok != 0 {
			return
		}
	}
	// Older Windows: system-wide awareness is still enough for the primary display.
	if p := user32.NewProc("SetProcessDPIAware"); p.Find() == nil {
		p.Call()
	}
}

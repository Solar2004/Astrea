//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Solar2004/Astrea/internal/procutil"
)

var procWTSQuerySessionInformation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

const wtsUserName = 5 // WTS_INFO_CLASS WTSUserName

// consoleUser names the user signed in at the physical console, "" if nobody
// (sign-in screen, or no console session at all).
func consoleUser() string {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xFFFFFFFF {
		return ""
	}
	var buf *uint16
	var n uint32
	r, _, _ := procWTSQuerySessionInformation.Call(0, uintptr(id), wtsUserName,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf)
}

// startGUITask runs the logon task that starts the gui helper in the
// signed-in user's session.
func startGUITask() error {
	cmd := exec.Command("schtasks", "/run", "/tn", guiTask)
	procutil.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

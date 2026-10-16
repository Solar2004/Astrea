//go:build windows

package main

// The "unlock" subcommand signs the console user back in from the Windows lock
// screen, for an account with no password. It is never invoked directly by a
// person: the session.unlock capability registers a one-shot scheduled task
// that runs this as SYSTEM (the agent itself runs as the desktop user and
// cannot reach the secure Winlogon desktop).
//
// Two phases of the same binary:
//
//	astrea-agent unlock             (as SYSTEM, session 0)
//	    duplicate the SYSTEM token, point it at the console session, and
//	    CreateProcessAsUser a child whose startup desktop is winsta0\winlogon.
//	astrea-agent unlock --desktop   (in the console session, on Winlogon)
//	    press the keys that dismiss the lock screen and sign in, then check the
//	    input desktop went back to Default. Writes the result JSON we read back.
//
// This mirrors how RustDesk drives the secure desktop. No CGo: everything goes
// through golang.org/x/sys/windows and a few user32 lazy procs.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/kbinani/screenshot"
	"golang.org/x/sys/windows"
)

var (
	uUser32                   = windows.NewLazySystemDLL("user32.dll")
	pSetThreadDesktop         = uUser32.NewProc("SetThreadDesktop")
	pCloseDesktop             = uUser32.NewProc("CloseDesktop")
	pOpenInputDesktop         = uUser32.NewProc("OpenInputDesktop")
	pGetUserObjectInformation = uUser32.NewProc("GetUserObjectInformationW")
	pKeybdEvent               = uUser32.NewProc("keybd_event")
	pGetForegroundWindow      = uUser32.NewProc("GetForegroundWindow")
	pGetWindowThreadProcessId = uUser32.NewProc("GetWindowThreadProcessId")
)

const (
	keyeventfKeyUp = 0x0002
	vkSpace        = 0x20
	vkReturn       = 0x0D
	vkEscape       = 0x1B
	desktopAll     = 0x000F01FF
	uoiName        = 2
)

// runUnlockCmd is the entry point for `astrea-agent unlock [...]`. It returns a
// process exit code. It always tries to leave a result file behind so the
// capability that launched it does not just time out.
func runUnlockCmd(args []string) int {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	desktop := fs.Bool("desktop", false, "internal: run on the Winlogon desktop in the console session")
	result := fs.String("result", "", "write the JSON result to this path")
	capture := fs.Bool("capture", false, "include a screenshot of the Winlogon desktop in the result")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logger := unlockLogger()
	if *desktop {
		return unlockDesktopPhase(logger, *result, *capture)
	}
	return unlockSystemPhase(logger, *result, *capture)
}

// enablePrivileges turns on the SYSTEM privileges needed to change a token's
// session and to assign it to a new process. Task Scheduler gives a SYSTEM task
// these privileges but leaves them disabled; SetTokenInformation and
// CreateProcessAsUser need them enabled.
func enablePrivileges(logger *log.Logger) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		logger.Printf("unlock: open token for privileges: %v", err)
		return
	}
	defer tok.Close()
	for _, name := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		var luid windows.LUID
		n, _ := windows.UTF16PtrFromString(name)
		if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
			logger.Printf("unlock: lookup %s: %v", name, err)
			continue
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
			logger.Printf("unlock: enable %s: %v", name, err)
		}
	}
}

func unlockLogger() *log.Logger {
	l := log.New(os.Stderr, "astrea-unlock ", log.LstdFlags)
	path := filepath.Join(programDataAstrea(), "agent.log")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		l.SetOutput(f)
	}
	return l
}

func programDataAstrea() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Astrea")
	}
	return filepath.Join(os.TempDir(), "Astrea")
}

func writeResult(path string, res map[string]any) {
	if path == "" {
		return
	}
	b, _ := json.Marshal(res)
	_ = os.WriteFile(path, b, 0o644)
}

// unlockSystemPhase runs as SYSTEM in session 0. It launches the desktop child
// inside the active console session, on the Winlogon secure desktop.
func unlockSystemPhase(logger *log.Logger, result string, capture bool) int {
	logger.Printf("unlock: SYSTEM phase starting")
	enablePrivileges(logger)
	sessionID := windows.WTSGetActiveConsoleSessionId()
	if sessionID == 0xFFFFFFFF {
		logger.Printf("unlock: no active console session")
		writeResult(result, map[string]any{"unlocked": false, "phase": "system", "note": "no active console session"})
		return 1
	}

	var token windows.Token
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY |
		windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &token); err != nil {
		return failSystem(logger, result, "OpenProcessToken", err)
	}
	defer token.Close()

	var dup windows.Token
	if err := windows.DuplicateTokenEx(token, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityIdentification, windows.TokenPrimary, &dup); err != nil {
		return failSystem(logger, result, "DuplicateTokenEx", err)
	}
	defer dup.Close()

	sid := sessionID
	if err := windows.SetTokenInformation(dup, windows.TokenSessionId,
		(*byte)(unsafe.Pointer(&sid)), uint32(unsafe.Sizeof(sid))); err != nil {
		return failSystem(logger, result, "SetTokenInformation(session)", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return failSystem(logger, result, "Executable", err)
	}
	cmdLine := fmt.Sprintf(`"%s" unlock --desktop --result "%s"`, exe, result)
	if capture {
		cmdLine += " --capture"
	}

	si := &windows.StartupInfo{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Desktop, _ = windows.UTF16PtrFromString(`winsta0\winlogon`)
	cmd16, _ := windows.UTF16PtrFromString(cmdLine)
	var pi windows.ProcessInformation

	if err := windows.CreateProcessAsUser(dup, nil, cmd16, nil, nil, false,
		windows.CREATE_NO_WINDOW, nil, nil, si, &pi); err != nil {
		return failSystem(logger, result, "CreateProcessAsUser", err)
	}
	logger.Printf("unlock: spawned desktop child pid=%d in session %d", pi.ProcessId, sessionID)
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	// Wait for the child to finish (it has its own time budget).
	windows.WaitForSingleObject(pi.Process, 90*1000)

	// If the child did not leave a result, record that we at least got here.
	if result != "" {
		if _, err := os.Stat(result); err != nil {
			writeResult(result, map[string]any{"unlocked": false, "phase": "system",
				"note": "desktop child exited without writing a result"})
		}
	}
	return 0
}

func failSystem(logger *log.Logger, result, step string, err error) int {
	logger.Printf("unlock: %s: %v", step, err)
	writeResult(result, map[string]any{"unlocked": false, "phase": "system", "note": step + ": " + err.Error()})
	return 1
}

// unlockDesktopPhase runs in the console session. The LockApp curtain lives on
// the Default desktop; dismissing it switches input to the secure Winlogon
// desktop where the sign-in UI appears. To make our keys land, we re-attach to
// whichever desktop currently receives input on every round. For a passwordless
// account a key dismisses the curtain and Enter signs straight back in.
func unlockDesktopPhase(logger *log.Logger, result string, capture bool) int {
	logger.Printf("unlock: desktop phase starting")
	res := map[string]any{"phase": "desktop"}

	var cur uintptr // the desktop handle currently assigned to this thread
	attach := func() {
		h, _, _ := pOpenInputDesktop.Call(0, 0, uintptr(desktopAll))
		if h == 0 {
			return
		}
		if r, _, _ := pSetThreadDesktop.Call(h); r == 0 {
			pCloseDesktop.Call(h)
			return
		}
		if cur != 0 {
			pCloseDesktop.Call(cur)
		}
		cur = h
	}
	defer func() {
		if cur != 0 {
			pCloseDesktop.Call(cur)
		}
	}()

	attach()
	res["desktop_before"] = inputDesktopName()
	res["foreground_before"] = foregroundProcess()
	logger.Printf("unlock: before: desktop=%q foreground=%q", inputDesktopName(), foregroundProcess())

	if capture {
		if img := captureWinlogon(logger); img != nil {
			res["images"] = []map[string]string{*img}
		}
	}

	unlocked := false
	for round := 0; round < 8 && !unlocked; round++ {
		attach() // follow the input desktop across the Default -> Winlogon switch
		tapKey(vkSpace)
		time.Sleep(500 * time.Millisecond)
		tapKey(vkReturn)
		time.Sleep(1500 * time.Millisecond)
		unlocked = !stillLocked()
		logger.Printf("unlock: round %d: desktop=%q foreground=%q unlocked=%v",
			round, inputDesktopName(), foregroundProcess(), unlocked)
	}

	attach()
	res["desktop_after"] = inputDesktopName()
	res["foreground_after"] = foregroundProcess()
	res["unlocked"] = unlocked
	if !unlocked {
		res["note"] = "sent keys but the PC still looks locked (lock screen or sign-in UI in front); " +
			"the account may have a password or a different sign-in UI"
	}
	writeResult(result, res)
	logger.Printf("unlock: desktop phase done, unlocked=%v", unlocked)
	return 0
}

// stillLocked reports whether the PC is still on the lock screen or sign-in UI:
// a secure desktop other than Default, or LockApp/LogonUI in the foreground.
func stillLocked() bool {
	if name := inputDesktopName(); name != "" && !strings.EqualFold(name, "Default") {
		return true
	}
	switch strings.ToLower(foregroundProcess()) {
	case "lockapp.exe", "logonui.exe":
		return true
	}
	return false
}

// tapKey presses and releases a virtual key via keybd_event (simple and works
// on the secure desktop once the thread is attached to it).
func tapKey(vk uintptr) {
	pKeybdEvent.Call(vk, 0, 0, 0)
	time.Sleep(40 * time.Millisecond)
	pKeybdEvent.Call(vk, 0, keyeventfKeyUp, 0)
}

// foregroundProcess returns the base name of the process owning the foreground
// window on the thread's current desktop (e.g. "LockApp.exe"), or "".
func foregroundProcess() string {
	hwnd, _, _ := pGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	pGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	name := make([]uint16, windows.MAX_PATH)
	size := uint32(len(name))
	if windows.QueryFullProcessImageName(h, 0, &name[0], &size) != nil {
		return ""
	}
	full := windows.UTF16ToString(name[:size])
	if i := strings.LastIndexAny(full, `\/`); i >= 0 {
		return full[i+1:]
	}
	return full
}

// inputDesktopName returns the name of the current input desktop (e.g.
// "Winlogon" on the secure desktop, "Default" once signed in).
func inputDesktopName() string {
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x0001) // DESKTOP_READOBJECTS
	if h == 0 {
		return ""
	}
	defer pCloseDesktop.Call(h)
	buf := make([]uint16, 64)
	var n uint32
	r, _, _ := pGetUserObjectInformation.Call(h, uoiName,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// captureWinlogon grabs a screenshot of the current (Winlogon) desktop and
// returns it as a capability image, or nil if capture failed.
func captureWinlogon(logger *log.Logger) *map[string]string {
	n := screenshot.NumActiveDisplays()
	if n == 0 {
		logger.Printf("unlock: capture: no active displays")
		return nil
	}
	bounds := screenshot.GetDisplayBounds(0)
	for i := 1; i < n; i++ {
		bounds = bounds.Union(screenshot.GetDisplayBounds(i))
	}
	shot, err := screenshot.CaptureRect(bounds)
	if err != nil {
		logger.Printf("unlock: capture: %v", err)
		return nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, shot, &jpeg.Options{Quality: 70}); err != nil {
		logger.Printf("unlock: capture encode: %v", err)
		return nil
	}
	return &map[string]string{
		"mime_type": "image/jpeg",
		"data":      base64.StdEncoding.EncodeToString(buf.Bytes()),
	}
}

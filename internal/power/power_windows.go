//go:build windows

package power

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	user32                      = windows.NewLazySystemDLL("user32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
	procGetTickCount            = kernel32.NewProc("GetTickCount")
	procGetLastInputInfo        = user32.NewProc("GetLastInputInfo")
	procPowerCreateRequest      = kernel32.NewProc("PowerCreateRequest")
	procPowerSetRequest         = kernel32.NewProc("PowerSetRequest")
	procPowerClearRequest       = kernel32.NewProc("PowerClearRequest")
	procGetForegroundWindow     = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW          = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcess  = user32.NewProc("GetWindowThreadProcessId")
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

// Power request types (POWER_REQUEST_TYPE).
const (
	powerRequestSystemRequired    = 1
	powerRequestExecutionRequired = 3
)

// reasonContext is REASON_CONTEXT with a simple string (the union is sized
// for its detailed variant).
type reasonContext struct {
	version uint32
	flags   uint32
	reason  *uint16
	_       [2]uintptr
}

// powerRequest is a handle-based power request: what Windows honours on
// Modern Standby PCs (S0 low-power idle), where the screen turning off puts
// the whole PC in standby and ES_SYSTEM_REQUIRED does not stop it.
// SystemRequired keeps it out of the low-power state, ExecutionRequired keeps
// our process from being suspended. On AC power neither expires; it shows in
// `powercfg /requests` under SYSTEM and EXECUTION.
var powerRequest = sync.OnceValue(func() uintptr {
	reason, _ := windows.UTF16PtrFromString("Astrea: keep this PC reachable for remote work")
	ctx := reasonContext{version: 0, flags: 1, reason: reason} // POWER_REQUEST_CONTEXT_SIMPLE_STRING
	h, _, _ := procPowerCreateRequest.Call(uintptr(unsafe.Pointer(&ctx)))
	if h == uintptr(windows.InvalidHandle) {
		return 0
	}
	return h
})

func setPowerRequest(on bool) error {
	h := powerRequest()
	if h == 0 {
		return errors.New("PowerCreateRequest failed")
	}
	proc := procPowerClearRequest
	if on {
		proc = procPowerSetRequest
	}
	for _, typ := range []uintptr{powerRequestSystemRequired, powerRequestExecutionRequired} {
		if r, _, err := proc.Call(h, typ); r == 0 && on {
			return err
		}
	}
	return nil
}

// inhibitor holds a power request and, as a second layer, ES_SYSTEM_REQUIRED
// from one locked OS thread: the execution state belongs to the calling
// thread, and Go would otherwise migrate us.
type inhibitor struct{ req chan inhibitReq }

type inhibitReq struct {
	on   bool
	done chan error
}

func newInhibitor() inhibitor {
	in := inhibitor{req: make(chan inhibitReq)}
	go func() {
		runtime.LockOSThread()
		for r := range in.req {
			flags := uintptr(esContinuous)
			if r.on {
				flags |= esSystemRequired
			}
			preqErr := setPowerRequest(r.on)
			ret, _, err := procSetThreadExecutionState.Call(flags)
			if ret == 0 && preqErr != nil {
				r.done <- errors.Join(preqErr, err)
				continue
			}
			r.done <- nil
		}
	}()
	return in
}

func (in inhibitor) set(on bool) error {
	done := make(chan error)
	in.req <- inhibitReq{on: on, done: done}
	return <-done
}

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

func idleTime() (time.Duration, error) {
	li := lastInputInfo{cbSize: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, err := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li))); r == 0 {
		return 0, err
	}
	now, _, _ := procGetTickCount.Call()
	return time.Duration(uint32(now)-li.dwTime) * time.Millisecond, nil
}

func foreground() (title, process string) {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return "", ""
	}
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	title = windows.UTF16ToString(buf[:n])
	var pid uint32
	procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid); err == nil {
		defer windows.CloseHandle(h)
		name := make([]uint16, windows.MAX_PATH)
		size := uint32(len(name))
		if windows.QueryFullProcessImageName(h, 0, &name[0], &size) == nil {
			process = filepath.Base(windows.UTF16ToString(name[:size]))
		}
	}
	return title, process
}

var (
	procOpenInputDesktop   = user32.NewProc("OpenInputDesktop")
	procCloseDesktop       = user32.NewProc("CloseDesktop")
	procGetUserObjectInfoW = user32.NewProc("GetUserObjectInformationW")
)

// screenLocked: the lock screen is up. LockApp.exe is the lock curtain; when
// the password/PIN box or a UAC prompt is showing, input goes to a secure
// desktop ("Winlogon") we cannot even open.
func screenLocked(process string) bool {
	if strings.EqualFold(process, "LockApp.exe") || strings.EqualFold(process, "LogonUI.exe") {
		return true
	}
	// Locked with the monitor asleep: the curtain is up but it is not the
	// foreground window, and the input desktop still reads "Default". A signed-in
	// desktop always has *some* foreground window (at worst Program Manager), so
	// none at all means the session is not usable — treat it as locked, or the
	// agent would hand back a black screenshot and never try to sign in.
	if hwnd, _, _ := procGetForegroundWindow.Call(); hwnd == 0 {
		return true
	}
	const desktopReadObjects = 0x0001
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return true
	}
	defer procCloseDesktop.Call(h)
	const uoiName = 2
	buf := make([]uint16, 64)
	var n uint32
	if r, _, _ := procGetUserObjectInfoW.Call(h, uoiName, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&n))); r == 0 {
		return false
	}
	return !strings.EqualFold(windows.UTF16ToString(buf), "Default")
}

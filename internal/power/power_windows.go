//go:build windows

package power

import (
	"path/filepath"
	"runtime"
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
	procGetForegroundWindow     = user32.NewProc("GetForegroundWindow")
	procGetWindowTextW          = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcess  = user32.NewProc("GetWindowThreadProcessId")
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

// inhibitor holds ES_SYSTEM_REQUIRED from one locked OS thread: the execution
// state belongs to the calling thread, and Go would otherwise migrate us.
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
			ret, _, err := procSetThreadExecutionState.Call(flags)
			if ret == 0 {
				r.done <- err
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

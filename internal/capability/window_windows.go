//go:build windows

package capability

import (
	"fmt"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() { winCtl = winWindows{} }

var (
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procIsIconic             = user32.NewProc("IsIconic")
	procIsZoomed             = user32.NewProc("IsZoomed")
	procShowWindow           = user32.NewProc("ShowWindow")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procPostMessageW         = user32.NewProc("PostMessageW")
	procGetWindow            = user32.NewProc("GetWindow")
	procGetWindowLongPtrW    = user32.NewProc("GetWindowLongPtrW")
)

const (
	swMaximize     = 3
	swMinimize     = 6
	swRestore      = 9
	wmClose        = 0x0010
	gwOwner        = 4
	gwlExStyle     = -20
	wsExToolWindow = 0x00000080
	wsExNoActivate = 0x08000000
	vkMenu         = 0x12
	maxWindowTitle = 512
)

type winWindows struct{}

// List walks the top-level windows in z-order (frontmost first) and keeps the
// ones Alt+Tab would show: visible, titled, unowned, not tool windows.
func (winWindows) List() ([]Window, error) {
	fg := windows.GetForegroundWindow()
	var out []Window
	cb := syscall.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		if !windows.IsWindowVisible(h) {
			return 1
		}
		if owner, _, _ := procGetWindow.Call(uintptr(h), gwOwner); owner != 0 {
			return 1
		}
		idx := int32(gwlExStyle)
		ex, _, _ := procGetWindowLongPtrW.Call(uintptr(h), uintptr(idx)) // sign-extended: GWL_EXSTYLE is -20
		if ex&(wsExToolWindow|wsExNoActivate) != 0 {
			return 1
		}
		title := windowTitle(h)
		if title == "" || h == windows.GetShellWindow() {
			return 1
		}
		var pid uint32
		_, _ = windows.GetWindowThreadProcessId(h, &pid)
		w := Window{Title: title, PID: pid, Process: processName(pid), State: "normal", Foreground: h == fg, handle: uintptr(h)}
		if r, _, _ := procIsIconic.Call(uintptr(h)); r != 0 {
			w.State = "minimized"
		} else if r, _, _ := procIsZoomed.Call(uintptr(h)); r != 0 {
			w.State = "maximized"
		}
		out = append(out, w)
		return 1
	})
	if err := windows.EnumWindows(cb, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func (winWindows) Set(w Window, action string) error {
	h := w.handle
	switch action {
	case "minimize":
		procShowWindow.Call(h, swMinimize)
	case "maximize":
		procShowWindow.Call(h, swMaximize)
		focusWindow(h)
	case "restore":
		procShowWindow.Call(h, swRestore)
		focusWindow(h)
	case "focus":
		if r, _, _ := procIsIconic.Call(h); r != 0 {
			procShowWindow.Call(h, swRestore)
		}
		if !focusWindow(h) {
			return fmt.Errorf("Windows refused to bring %q to the front", w.Title)
		}
	case "close":
		if r, _, err := procPostMessageW.Call(h, wmClose, 0, 0); r == 0 {
			return fmt.Errorf("close %q: %v", w.Title, err)
		}
	}
	return nil
}

// focusWindow brings h to the front. Windows only lets the process that got
// the last input do that, so tap Alt first (the usual workaround: our
// SendInput counts as that input).
func focusWindow(h uintptr) bool {
	for _, flags := range []uint32{0, keyeventfKeyUp} {
		in := input32{typ: inputKeyboard}
		kb := (*keybdInput)(unsafe.Pointer(&in.u))
		kb.wVk = vkMenu
		kb.dwFlags = flags
		_ = sendInput(&in)
	}
	procSetForegroundWindow.Call(h)
	for i := 0; i < 10; i++ {
		if uintptr(windows.GetForegroundWindow()) == h {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

func windowTitle(h windows.HWND) string {
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, min(int(n)+1, maxWindowTitle))
	procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func processName(pid uint32) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &n); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

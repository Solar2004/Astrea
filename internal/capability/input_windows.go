//go:build windows

package capability

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() { input = winInput{} }

// winInput drives the mouse and keyboard through the Win32 SendInput API.
// No CGo: all calls go through the syscall-backed golang.org/x/sys/windows.
type winInput struct{}

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	procSendInput     = user32.NewProc("SendInput")
	procSetCursorPos  = user32.NewProc("SetCursorPos")
	procGetSystemMets = user32.NewProc("GetSystemMetrics")
)

// INPUT type constants.
const (
	inputMouse    = 0
	inputKeyboard = 1
)

// Mouse event flags.
const (
	mouseeventfMove        = 0x0001
	mouseeventfAbsolute    = 0x8000
	mouseeventfVirtualDesk = 0x4000
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
)

// Keyboard event flags.
const (
	keyeventfKeyUp   = 0x0002
	keyeventfUnicode = 0x0004
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// INPUT structs mirror the Win32 layout. mouseKbdInput is a union large enough
// for both MOUSEINPUT and KEYBDINPUT.
type input32 struct {
	typ uint32
	_   uint32 // padding to align the union on 64-bit
	u   mouseKbdInput
}

type mouseKbdInput struct {
	dx          int32
	dy          int32
	mouseData   uint32
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
	_           [8]byte // tail padding so size matches the larger union member
}

func sendInput(in *input32) {
	procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
}

func getSystemMetric(index int) int {
	r, _, _ := procGetSystemMets.Call(uintptr(index))
	return int(int32(r))
}

func (winInput) MouseMove(x, y int) error {
	// Prefer SetCursorPos for exact pixel placement in screen coordinates.
	procSetCursorPos.Call(uintptr(x), uintptr(y))
	return nil
}

func (winInput) MouseClick(button string, double bool) error {
	var down, up uint32
	switch button {
	case "right":
		down, up = mouseeventfRightDown, mouseeventfRightUp
	case "middle":
		down, up = mouseeventfMiddleDown, mouseeventfMiddleUp
	default:
		down, up = mouseeventfLeftDown, mouseeventfLeftUp
	}
	click := func() {
		in := input32{typ: inputMouse}
		in.u.dwFlags = down
		sendInput(&in)
		in.u.dwFlags = up
		sendInput(&in)
	}
	click()
	if double {
		time.Sleep(40 * time.Millisecond)
		click()
	}
	return nil
}

func (winInput) KeyType(text string) error {
	for _, r := range text {
		// Send each rune as a Unicode keystroke (down then up).
		for _, flags := range []uint32{keyeventfUnicode, keyeventfUnicode | keyeventfKeyUp} {
			in := input32{typ: inputKeyboard}
			// For keyboard input, reuse the union: dx holds vk/scan packed.
			// Layout: wVk(0), wScan(rune), dwFlags, time, extra.
			kb := (*keybdInput)(unsafe.Pointer(&in.u))
			kb.wVk = 0
			kb.wScan = uint16(r)
			kb.dwFlags = flags
			sendInput(&in)
		}
	}
	return nil
}

// keybdInput overlays mouseKbdInput for keyboard events.
type keybdInput struct {
	wVk         uint16
	wScan       uint16
	dwFlags     uint32
	time        uint32
	dwExtraInfo uintptr
}

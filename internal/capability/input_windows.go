//go:build windows

package capability

import (
	"fmt"
	"strings"
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

// INPUT structs mirror the Win32 layout: 40 bytes on 64-bit. mouseKbdInput is
// MOUSEINPUT, the largest member of the union, so KEYBDINPUT fits inside it.
// SendInput rejects every event if cbSize is not exactly sizeof(INPUT).
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
}

// sendInput fails when Windows did not inject the event (wrong size, input
// blocked by UIPI or a secure desktop such as the lock screen or UAC).
func sendInput(in *input32) error {
	n, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
	if n != 1 {
		return fmt.Errorf("SendInput: %v (¿pantalla bloqueada o ventana de administrador?)", err)
	}
	return nil
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
	click := func() error {
		in := input32{typ: inputMouse}
		in.u.dwFlags = down
		if err := sendInput(&in); err != nil {
			return err
		}
		in.u.dwFlags = up
		return sendInput(&in)
	}
	if err := click(); err != nil || !double {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	return click()
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
			if err := sendInput(&in); err != nil {
				return err
			}
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

const (
	mouseeventfWheel  = 0x0800
	mouseeventfHWheel = 0x1000
	keyeventfExtended = 0x0001
	wheelDelta        = 120
)

func (winInput) MouseButton(button string, down bool) error {
	flag := map[string][2]uint32{
		"left":   {mouseeventfLeftDown, mouseeventfLeftUp},
		"right":  {mouseeventfRightDown, mouseeventfRightUp},
		"middle": {mouseeventfMiddleDown, mouseeventfMiddleUp},
	}[button]
	if flag[0] == 0 {
		flag = [2]uint32{mouseeventfLeftDown, mouseeventfLeftUp}
	}
	in := input32{typ: inputMouse}
	in.u.dwFlags = flag[1]
	if down {
		in.u.dwFlags = flag[0]
	}
	return sendInput(&in)
}

func (winInput) Scroll(dx, dy int) error {
	if dy != 0 {
		in := input32{typ: inputMouse}
		in.u.dwFlags = mouseeventfWheel
		in.u.mouseData = uint32(int32(-dy * wheelDelta)) // positive = away from the user (up)
		if err := sendInput(&in); err != nil {
			return err
		}
	}
	if dx != 0 {
		in := input32{typ: inputMouse}
		in.u.dwFlags = mouseeventfHWheel
		in.u.mouseData = uint32(int32(dx * wheelDelta))
		return sendInput(&in)
	}
	return nil
}

// winVK maps key.press names to virtual-key codes; ext marks extended keys.
var winVK = map[string]uint16{
	"ctrl": 0x11, "shift": 0x10, "alt": 0x12, "win": 0x5B, "enter": 0x0D, "esc": 0x1B,
	"tab": 0x09, "backspace": 0x08, "space": 0x20, "delete": 0x2E, "insert": 0x2D,
	"home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22, "left": 0x25, "up": 0x26,
	"right": 0x27, "down": 0x28, "capslock": 0x14, "printscreen": 0x2C, "menu": 0x5D,
}

var winExtended = map[string]bool{"delete": true, "insert": true, "home": true, "end": true,
	"pageup": true, "pagedown": true, "left": true, "up": true, "right": true, "down": true,
	"win": true, "menu": true, "printscreen": true}

func vkOf(k string) uint16 {
	if vk, ok := winVK[k]; ok {
		return vk
	}
	if len(k) == 1 { // a-z, 0-9: the VK is the uppercase ASCII code
		return uint16(strings.ToUpper(k)[0])
	}
	var n int
	if _, err := fmt.Sscanf(k, "f%d", &n); err == nil && n >= 1 && n <= 12 {
		return uint16(0x70 + n - 1)
	}
	return 0
}

func (winInput) KeyPress(keys []string) error {
	var first error
	send := func(k string, up bool) {
		in := input32{typ: inputKeyboard}
		kb := (*keybdInput)(unsafe.Pointer(&in.u))
		kb.wVk = vkOf(k)
		if winExtended[k] {
			kb.dwFlags |= keyeventfExtended
		}
		if up {
			kb.dwFlags |= keyeventfKeyUp
		}
		if err := sendInput(&in); err != nil && first == nil {
			first = err
		}
	}
	for _, k := range keys {
		send(k, false)
	}
	for i := len(keys) - 1; i >= 0; i-- { // release even if a press failed
		send(keys[i], true)
	}
	return first
}

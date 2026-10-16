//go:build windows

package capability

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() { recorder = &winRecorder{} }

var (
	procSetWindowsHookEx   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHook  = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx     = user32.NewProc("CallNextHookEx")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procPostThreadMessageW = user32.NewProc("PostThreadMessageW")
	procGetKeyState        = user32.NewProc("GetKeyState")
	kernel32rec            = windows.NewLazySystemDLL("kernel32.dll")
	procGetCurrentThreadID = kernel32rec.NewProc("GetCurrentThreadId")
)

const (
	whMouseLL    = 14
	whKeyboardLL = 13

	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A

	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105
	wmQuit       = 0x0012

	llmhfInjected = 0x01
	llkhfInjected = 0x10
)

type msllhookstruct struct {
	ptX, ptY    int32
	mouseData   uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

type kbdllhookstruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

// winRecorder captures global input with low-level hooks on a dedicated
// message-loop thread. Astrea's own injected events (LL*_INJECTED) are skipped.
type winRecorder struct {
	mu       sync.Mutex
	on       bool
	start    time.Time
	events   []RecEvent
	threadID uint32
	done     chan struct{}

	// typing accumulates printable keystrokes into one key.type step.
	typing strings.Builder
}

var activeRec *winRecorder // the callbacks reach the recorder through this

func (r *winRecorder) Recording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.on
}

func (r *winRecorder) Start() error {
	r.mu.Lock()
	if r.on {
		r.mu.Unlock()
		return fmt.Errorf("already recording")
	}
	r.on = true
	r.start = time.Now()
	r.events = nil
	r.typing.Reset()
	r.done = make(chan struct{})
	r.mu.Unlock()
	activeRec = r

	ready := make(chan error, 1)
	go r.loop(ready)
	return <-ready
}

func (r *winRecorder) Stop() ([]RecEvent, error) {
	r.mu.Lock()
	if !r.on {
		r.mu.Unlock()
		return nil, fmt.Errorf("not recording")
	}
	tid := r.threadID
	done := r.done
	r.mu.Unlock()

	// Ask the message loop to quit, then wait for it to tear the hooks down.
	procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushTypingLocked(time.Now())
	r.on = false
	activeRec = nil
	return r.events, nil
}

// loop installs the hooks and runs the Windows message pump. The thread is
// locked because low-level hooks deliver to the thread that set them.
func (r *winRecorder) loop(ready chan<- error) {
	// This goroutine owns the OS thread for the hook lifetime.
	tid, _, _ := procGetCurrentThreadID.Call()
	r.mu.Lock()
	r.threadID = uint32(tid)
	r.mu.Unlock()

	mouseCB := windows.NewCallback(mouseProc)
	keyCB := windows.NewCallback(keyProc)
	hMouse, _, err := procSetWindowsHookEx.Call(whMouseLL, mouseCB, 0, 0)
	if hMouse == 0 {
		ready <- fmt.Errorf("SetWindowsHookEx(mouse): %v", err)
		return
	}
	hKey, _, err := procSetWindowsHookEx.Call(whKeyboardLL, keyCB, 0, 0)
	if hKey == 0 {
		procUnhookWindowsHook.Call(hMouse)
		ready <- fmt.Errorf("SetWindowsHookEx(keyboard): %v", err)
		return
	}
	ready <- nil

	var m msg
	for {
		r2, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r2) <= 0 { // WM_QUIT (0) or error (-1)
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	procUnhookWindowsHook.Call(hMouse)
	procUnhookWindowsHook.Call(hKey)
	close(r.done)
}

func (r *winRecorder) ms() int64 { return time.Since(r.start).Milliseconds() }

func (r *winRecorder) flushTypingLocked(_ time.Time) {
	if r.typing.Len() > 0 {
		r.events = append(r.events, RecEvent{T: r.ms(), Kind: "type", Text: r.typing.String()})
		r.typing.Reset()
	}
}

func mouseProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	r := activeRec
	if nCode < 0 || r == nil {
		return callNext(nCode, wParam, lParam)
	}
	s := (*msllhookstruct)(unsafe.Pointer(lParam))
	if s.flags&llmhfInjected == 0 { // ignore Astrea's own clicks
		r.mu.Lock()
		switch wParam {
		case wmLButtonDown, wmRButtonDown, wmMButtonDown:
			r.flushTypingLocked(time.Now())
			btn := map[uintptr]string{wmLButtonDown: "left", wmRButtonDown: "right", wmMButtonDown: "middle"}[wParam]
			r.events = append(r.events, RecEvent{T: r.ms(), Kind: "down", X: int(s.ptX), Y: int(s.ptY), Btn: btn})
		case wmMouseWheel:
			r.flushTypingLocked(time.Now())
			delta := int16(uint16(s.mouseData >> 16))
			r.events = append(r.events, RecEvent{T: r.ms(), Kind: "wheel", Dy: int(delta) / 120})
		}
		r.mu.Unlock()
	}
	return callNext(nCode, wParam, lParam)
}

func keyProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	r := activeRec
	if nCode < 0 || r == nil {
		return callNext(nCode, wParam, lParam)
	}
	k := (*kbdllhookstruct)(unsafe.Pointer(lParam))
	if (wParam == wmKeyDown || wParam == wmSysKeyDown) && k.flags&llkhfInjected == 0 {
		r.recordKey(uint16(k.vkCode))
	}
	return callNext(nCode, wParam, lParam)
}

func callNext(nCode int, w, l uintptr) uintptr {
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), w, l)
	return r
}

// recordKey turns a keydown into either accumulated text (plain character) or
// a key.press combo (anything with a non-shift modifier, or a named key).
func (r *winRecorder) recordKey(vk uint16) {
	if isModifierVK(vk) {
		return // modifiers are captured as part of the combo they belong to
	}
	ctrl := down(0x11)
	alt := down(0x12)
	win := down(0x5B) || down(0x5C)
	shift := down(0x10)

	name := vkName(vk)
	r.mu.Lock()
	defer r.mu.Unlock()

	// A plain printable key (no ctrl/alt/win): accumulate as typed text.
	if !ctrl && !alt && !win {
		if ch := printable(vk, shift); ch != 0 {
			r.typing.WriteRune(ch)
			return
		}
	}
	r.flushTypingLocked(time.Now())
	if name == "" {
		return
	}
	var parts []string
	if ctrl {
		parts = append(parts, "ctrl")
	}
	if alt {
		parts = append(parts, "alt")
	}
	if win {
		parts = append(parts, "win")
	}
	if shift {
		parts = append(parts, "shift")
	}
	parts = append(parts, name)
	r.events = append(r.events, RecEvent{T: r.ms(), Kind: "key", Keys: strings.Join(parts, "+")})
}

func down(vk uint16) bool {
	r, _, _ := procGetKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

func isModifierVK(vk uint16) bool {
	switch vk {
	case 0x10, 0x11, 0x12, 0x5B, 0x5C, 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5:
		return true
	}
	return false
}

// vkName is the reverse of vkOf (input_windows.go): a VK code to a key.press name.
var vkNames = func() map[uint16]string {
	m := map[uint16]string{}
	for name, vk := range winVK {
		if !isModifierVK(vk) {
			m[vk] = name
		}
	}
	return m
}()

func vkName(vk uint16) string {
	if n, ok := vkNames[vk]; ok {
		return n
	}
	if vk >= '0' && vk <= '9' {
		return string(rune(vk))
	}
	if vk >= 'A' && vk <= 'Z' {
		return strings.ToLower(string(rune(vk)))
	}
	if vk >= 0x70 && vk <= 0x7B {
		return fmt.Sprintf("f%d", vk-0x70+1)
	}
	return ""
}

// printable returns the character a plain key produces (letters/digits/space),
// honoring shift for letters. Punctuation layouts vary by locale, so anything
// not clearly a letter/digit/space is left to key.press.
func printable(vk uint16, shift bool) rune {
	switch {
	case vk >= 'A' && vk <= 'Z':
		if shift {
			return rune(vk)
		}
		return rune(vk + 32)
	case vk >= '0' && vk <= '9':
		if shift {
			return 0 // shifted digits are symbols: locale-dependent
		}
		return rune(vk)
	case vk == 0x20:
		return ' '
	}
	return 0
}

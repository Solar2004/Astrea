//go:build windows

package power

import (
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetModuleHandleW  = kernel32.NewProc("GetModuleHandleW")
	procSetWindowsHookExW = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx    = user32.NewProc("CallNextHookEx")
	procGetMessageW       = user32.NewProc("GetMessageW")
)

const (
	whKeyboardLL = 13
	whMouseLL    = 14

	wmKeyUp       = 0x0101
	wmSysKeyUp    = 0x0105
	wmMouseMove   = 0x0200
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A
	wmXButtonUp   = 0x020C
	wmMouseHWheel = 0x020E

	llmhfInjected = 0x01 | 0x02 // LLMHF_INJECTED | LLMHF_LOWER_IL_INJECTED
	llkhfInjected = 0x10 | 0x02 // LLKHF_INJECTED | LLKHF_LOWER_IL_INJECTED
)

type msllHook struct {
	x, y      int32
	mouseData uint32
	flags     uint32
	time      uint32
	extra     uintptr
}

type kbdllHook struct {
	vkCode, scanCode, flags, time uint32 // vkCode is never read
	extra                         uintptr
}

type winMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      [2]int32
	private uint32
}

// startHooks installs low-level mouse and keyboard hooks on a dedicated OS
// thread (they are called on the installing thread's message loop). Windows
// flags input made with SendInput — Astrea's own, jigglers, remote tools — as
// injected. The callbacks only take a timestamp and must return fast: Windows
// silently drops a hook that keeps it waiting.
func startHooks(t *tracker) bool {
	ok := make(chan bool)
	go func() {
		runtime.LockOSThread()
		mouse := windows.NewCallback(func(code, wp, lp uintptr) uintptr {
			if int32(code) >= 0 {
				h := *(**msllHook)(unsafe.Pointer(&lp))
				inj, now := h.flags&llmhfInjected != 0, time.Now()
				t.mu.Lock()
				switch wp {
				case wmMouseMove:
					t.move(now, float64(h.x), float64(h.y), inj)
				case wmLButtonUp, wmRButtonUp, wmMButtonUp, wmXButtonUp, wmMouseWheel, wmMouseHWheel:
					t.act(now, inj)
				default:
					t.seen(now, inj)
				}
				t.mu.Unlock()
			}
			r, _, _ := procCallNextHookEx.Call(0, code, wp, lp)
			return r
		})
		keys := windows.NewCallback(func(code, wp, lp uintptr) uintptr {
			if int32(code) >= 0 {
				h := *(**kbdllHook)(unsafe.Pointer(&lp))
				inj, now := h.flags&llkhfInjected != 0, time.Now()
				t.mu.Lock()
				if wp == wmKeyUp || wp == wmSysKeyUp {
					t.act(now, inj) // a held key repeats downs but releases once
				} else {
					t.seen(now, inj)
				}
				t.mu.Unlock()
			}
			r, _, _ := procCallNextHookEx.Call(0, code, wp, lp)
			return r
		})
		mod, _, _ := procGetModuleHandleW.Call(0)
		hm, _, _ := procSetWindowsHookExW.Call(whMouseLL, mouse, mod, 0)
		hk, _, _ := procSetWindowsHookExW.Call(whKeyboardLL, keys, mod, 0)
		ok <- hm != 0 && hk != 0
		if hm == 0 && hk == 0 {
			return
		}
		var m winMsg
		for {
			if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
				return
			}
		}
	}()
	return <-ok
}

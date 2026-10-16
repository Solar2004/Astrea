//go:build windows

package capability

import (
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() { clip = winClip{} }

// winClip uses the Win32 clipboard API directly (no CGo).
type winClip struct{}

var (
	procOpenClipboard         = user32.NewProc("OpenClipboard")
	procCloseClipboard        = user32.NewProc("CloseClipboard")
	procEmptyClipboard        = user32.NewProc("EmptyClipboard")
	procGetClipboardData      = user32.NewProc("GetClipboardData")
	procSetClipboardData      = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvai = user32.NewProc("IsClipboardFormatAvailable")
	procGetClipboardSeq       = user32.NewProc("GetClipboardSequenceNumber")

	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	cfDIB         = 8
	gmemMoveable  = 0x0002
)

// open holds the clipboard on this OS thread (another program may have it
// open for a moment: retry briefly). The caller must close on the same thread.
func (winClip) open() error {
	var err error
	for i := 0; i < 20; i++ {
		r, _, e := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		err = e
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("OpenClipboard: %v (another program is holding the clipboard)", err)
}

func (c winClip) Text() (string, bool, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := c.open(); err != nil {
		return "", false, err
	}
	defer procCloseClipboard.Call()
	if r, _, _ := procIsClipboardFormatAvai.Call(cfUnicodeText); r == 0 {
		img, _, _ := procIsClipboardFormatAvai.Call(cfDIB)
		return "", img != 0, nil
	}
	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", false, fmt.Errorf("GetClipboardData: %v", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		return "", false, fmt.Errorf("GlobalLock: %v", err)
	}
	defer procGlobalUnlock.Call(h)
	// p is an address returned by GlobalLock; reinterpret the uintptr's bits as
	// a *uint16 without a direct uintptr→Pointer conversion (go vet).
	ptr := *(**uint16)(unsafe.Pointer(&p))
	return windows.UTF16PtrToString(ptr), false, nil
}

func (c winClip) SetText(text string) error {
	u, err := windows.UTF16FromString(text)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := c.open(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	size := uintptr(len(u) * 2)
	h, _, err := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return fmt.Errorf("GlobalAlloc: %v", err)
	}
	p, _, err := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("GlobalLock: %v", err)
	}
	dst := *(**uint16)(unsafe.Pointer(&p))
	copy(unsafe.Slice(dst, len(u)), u)
	procGlobalUnlock.Call(h)
	if r, _, err := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h) // the system did not take ownership
		return fmt.Errorf("SetClipboardData: %v", err)
	}
	return nil
}

func (winClip) Seq() uint64 {
	r, _, _ := procGetClipboardSeq.Call()
	return uint64(r)
}

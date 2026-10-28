//go:build windows

package capability

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows audio backend: WASAPI captures the default recording device and Core
// Audio's IAudioEndpointVolume reads and sets the mic level. No CGo, no extra
// DLLs — the same vtable-call style as gpu_windows.go (comCall / vtRelease).

func init() { audioDev = winAudio{} }

var (
	ole32DLL           = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx = ole32DLL.NewProc("CoInitializeEx")
	procCoUninitialize = ole32DLL.NewProc("CoUninitialize")
	procCoCreateInst   = ole32DLL.NewProc("CoCreateInstance")
	procCoTaskMemFree  = ole32DLL.NewProc("CoTaskMemFree")
)

// GUIDs for the Core Audio / WASAPI interfaces.
var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C,
		Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35,
		Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32,
		Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0,
		Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
	iidIAudioEndpointVolume = windows.GUID{Data1: 0x5CDF2C82, Data2: 0x841E, Data3: 0x4546,
		Data4: [8]byte{0x97, 0x22, 0x0C, 0xF7, 0x40, 0x78, 0x22, 0x9A}}
)

const (
	clsctxAll    = 0x17
	coinitMT     = 0x0        // COINIT_MULTITHREADED
	eCapture     = 1          // EDataFlow
	eConsole     = 0          // ERole
	shareShared  = 0          // AUDCLNT_SHAREMODE_SHARED
	flagSilent   = 0x2        // AUDCLNT_BUFFERFLAGS_SILENT
	refHundredNs = 10_000_000 // 1 second, in 100ns units
)

// waveFormatEx mirrors the WAVEFORMATEX header GetMixFormat returns. For an
// EXTENSIBLE format the 16-byte SubFormat GUID follows the fixed fields; its
// Data1 is 1 for PCM, 3 for IEEE float.
type waveFormatEx struct {
	wFormatTag      uint16
	nChannels       uint16
	nSamplesPerSec  uint32
	nAvgBytesPerSec uint32
	nBlockAlign     uint16
	wBitsPerSample  uint16
	cbSize          uint16
}

type winAudio struct{}

// comUp initialises COM on the current (locked) OS thread and returns a cleanup
// func. COM calls must stay on the thread that initialised it.
func comUp() func() {
	runtime.LockOSThread()
	hr, _, _ := procCoInitializeEx.Call(0, coinitMT)
	// S_OK (0) or S_FALSE (1): we own the init and must balance it. Any other
	// value (e.g. RPC_E_CHANGED_MODE) means COM was already up in another mode;
	// proceed without claiming the uninit.
	owned := hr == 0 || hr == 1
	return func() {
		if owned {
			procCoUninitialize.Call()
		}
		runtime.UnlockOSThread()
	}
}

func hrOK(hr uintptr) bool { return int32(hr) >= 0 }

// hrErr names the failed call and its HRESULT, so a field report is actionable.
func hrErr(call string, hr uintptr) error {
	return fmt.Errorf("%s failed (hr=0x%08X)", call, uint32(hr))
}

// enumerator creates an IMMDeviceEnumerator. The caller releases it.
func enumerator() (unsafe.Pointer, error) {
	var enum unsafe.Pointer
	hr, _, _ := procCoCreateInst.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enum)))
	if !hrOK(hr) || enum == nil {
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator) failed (hr=0x%08X); is the Windows Audio service running?", uint32(hr))
	}
	return enum, nil
}

// defaultCaptureDevice returns the default recording IMMDevice. The caller
// releases it.
func defaultCaptureDevice(enum unsafe.Pointer) (unsafe.Pointer, error) {
	var dev unsafe.Pointer
	// GetDefaultAudioEndpoint(dataFlow, role, **device) — vtable slot 4.
	hr := comCall(enum, 4, eCapture, eConsole, uintptr(unsafe.Pointer(&dev)))
	if !hrOK(hr) || dev == nil {
		return nil, errors.New("no default recording device (is a microphone connected and enabled?)")
	}
	return dev, nil
}

// endpointVolume activates IAudioEndpointVolume on dev. The caller releases it.
func endpointVolume(dev unsafe.Pointer) (unsafe.Pointer, error) {
	var vol unsafe.Pointer
	// IMMDevice::Activate(iid, clsctx, params, **iface) — vtable slot 3.
	hr := comCall(dev, 3, uintptr(unsafe.Pointer(&iidIAudioEndpointVolume)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&vol)))
	if !hrOK(hr) || vol == nil {
		return nil, hrErr("Activate(IAudioEndpointVolume)", hr)
	}
	return vol, nil
}

func (winAudio) MicLevel() (int, bool, error) {
	done := comUp()
	defer done()
	enum, err := enumerator()
	if err != nil {
		return 0, false, err
	}
	defer comCall(enum, vtRelease)
	dev, err := defaultCaptureDevice(enum)
	if err != nil {
		return 0, false, err
	}
	defer comCall(dev, vtRelease)
	vol, err := endpointVolume(dev)
	if err != nil {
		return 0, false, err
	}
	defer comCall(vol, vtRelease)

	var scalar float32
	// GetMasterVolumeLevelScalar(*float) — slot 9.
	if hr := comCall(vol, 9, uintptr(unsafe.Pointer(&scalar))); !hrOK(hr) {
		return 0, false, hrErr("GetMasterVolumeLevelScalar", hr)
	}
	var muted int32
	// GetMute(*BOOL) — slot 15.
	comCall(vol, 15, uintptr(unsafe.Pointer(&muted)))
	return int(math.Round(float64(scalar) * 100)), muted != 0, nil
}

func (winAudio) SetMicLevel(level int, mute *bool) error {
	done := comUp()
	defer done()
	enum, err := enumerator()
	if err != nil {
		return err
	}
	defer comCall(enum, vtRelease)
	dev, err := defaultCaptureDevice(enum)
	if err != nil {
		return err
	}
	defer comCall(dev, vtRelease)
	vol, err := endpointVolume(dev)
	if err != nil {
		return err
	}
	defer comCall(vol, vtRelease)

	if level >= 0 {
		scalar := float32(level) / 100
		// SetMasterVolumeLevelScalar(float, guid*) — slot 7. The float goes in
		// XMM1: SyscallN copies the first args into the XMM registers too.
		if hr := comCall(vol, 7, uintptr(math.Float32bits(scalar)), 0); !hrOK(hr) {
			return hrErr("SetMasterVolumeLevelScalar", hr)
		}
	}
	if mute != nil {
		var b uintptr
		if *mute {
			b = 1
		}
		// SetMute(BOOL, guid*) — slot 14.
		if hr := comCall(vol, 14, b, 0); !hrOK(hr) {
			return hrErr("SetMute", hr)
		}
	}
	return nil
}

func (winAudio) Capture(ctx context.Context, seconds int) ([]int16, int, int, error) {
	done := comUp()
	defer done()
	enum, err := enumerator()
	if err != nil {
		return nil, 0, 0, err
	}
	defer comCall(enum, vtRelease)
	dev, err := defaultCaptureDevice(enum)
	if err != nil {
		return nil, 0, 0, err
	}
	defer comCall(dev, vtRelease)

	var client unsafe.Pointer
	// IMMDevice::Activate(IID_IAudioClient) — slot 3.
	if hr := comCall(dev, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0,
		uintptr(unsafe.Pointer(&client))); !hrOK(hr) || client == nil {
		return nil, 0, 0, hrErr("Activate(IAudioClient)", hr)
	}
	defer comCall(client, vtRelease)

	var pFormat *waveFormatEx
	// IAudioClient::GetMixFormat(**WAVEFORMATEX) — slot 8.
	if hr := comCall(client, 8, uintptr(unsafe.Pointer(&pFormat))); !hrOK(hr) || pFormat == nil {
		return nil, 0, 0, hrErr("GetMixFormat", hr)
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(pFormat)))
	fmtTag := pFormat.wFormatTag
	channels := int(pFormat.nChannels)
	rate := int(pFormat.nSamplesPerSec)
	bits := int(pFormat.wBitsPerSample)
	isFloat, err := formatKind(pFormat)
	if err != nil {
		return nil, 0, 0, err
	}

	// IAudioClient::Initialize(SHARED, flags=0, hnsBufferDuration=1s, 0, fmt, nil) — slot 3.
	if hr := comCall(client, 3, shareShared, 0, refHundredNs, 0,
		uintptr(unsafe.Pointer(pFormat)), 0); !hrOK(hr) {
		return nil, 0, 0, hrErr("IAudioClient.Initialize", hr)
	}

	var capClient unsafe.Pointer
	// IAudioClient::GetService(IID_IAudioCaptureClient) — slot 14.
	if hr := comCall(client, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)),
		uintptr(unsafe.Pointer(&capClient))); !hrOK(hr) || capClient == nil {
		return nil, 0, 0, hrErr("GetService(IAudioCaptureClient)", hr)
	}
	defer comCall(capClient, vtRelease)

	if hr := comCall(client, 10); !hrOK(hr) { // Start — slot 10.
		return nil, 0, 0, hrErr("IAudioClient.Start", hr)
	}
	defer comCall(client, 11) // Stop — slot 11.

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	out := make([]int16, 0, rate*channels*seconds)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		var next uint32
		// GetNextPacketSize(*UINT32) — slot 5.
		comCall(capClient, 5, uintptr(unsafe.Pointer(&next)))
		for next > 0 {
			var pData *byte
			var numFrames, flags uint32
			// GetBuffer(**data, *frames, *flags, *devpos, *qpcpos) — slot 3.
			hr := comCall(capClient, 3, uintptr(unsafe.Pointer(&pData)),
				uintptr(unsafe.Pointer(&numFrames)), uintptr(unsafe.Pointer(&flags)), 0, 0)
			if !hrOK(hr) {
				break
			}
			if numFrames > 0 {
				n := int(numFrames) * channels
				if flags&flagSilent != 0 || pData == nil {
					out = append(out, make([]int16, n)...)
				} else {
					out = appendSamples(out, pData, n, isFloat, bits)
				}
			}
			// ReleaseBuffer(frames) — slot 4.
			comCall(capClient, 4, uintptr(numFrames))
			comCall(capClient, 5, uintptr(unsafe.Pointer(&next)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = fmtTag
	return out, rate, channels, nil
}

// formatKind reports whether the mix format is IEEE float (vs 16-bit PCM),
// resolving an EXTENSIBLE tag through its SubFormat GUID.
func formatKind(f *waveFormatEx) (isFloat bool, err error) {
	const (
		wavePCM        = 1
		waveIEEEFloat  = 3
		waveExtensible = 0xFFFE
	)
	tag := f.wFormatTag
	if tag == waveExtensible {
		// SubFormat GUID follows the 18-byte WAVEFORMATEX + wValidBits(2) +
		// dwChannelMask(4) = offset 24. Its Data1 is the real format tag.
		base := unsafe.Pointer(f)
		sub := (*windows.GUID)(unsafe.Add(base, 24))
		tag = uint16(sub.Data1)
	}
	switch tag {
	case waveIEEEFloat:
		return true, nil
	case wavePCM:
		if f.wBitsPerSample != 16 {
			return false, errors.New("unsupported mic format (only 16-bit PCM or float)")
		}
		return false, nil
	default:
		return false, errors.New("unsupported mic sample format")
	}
}

// appendSamples converts n interleaved samples at p to int16 and appends them.
func appendSamples(out []int16, p *byte, n int, isFloat bool, bits int) []int16 {
	if isFloat {
		src := unsafe.Slice((*float32)(unsafe.Pointer(p)), n)
		for _, f := range src {
			v := math.Round(float64(f) * 32767)
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			out = append(out, int16(v))
		}
		return out
	}
	// 16-bit PCM.
	src := unsafe.Slice((*int16)(unsafe.Pointer(p)), n)
	return append(out, src...)
}

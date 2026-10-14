//go:build windows

package capability

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GPUs on Windows: DXGI lists the adapters (name, vendor, real VRAM size and
// the LUID that identifies them), and the "GPU Engine" / "GPU Adapter Memory"
// performance counters — the ones Task Manager uses — give live usage per LUID.
// nvidia-smi, when present, adds temperature for NVIDIA cards. No CGo, no WMI.

var (
	dxgiDLL                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")

	pdhDLL                           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhCloseQuery                = pdhDLL.NewProc("PdhCloseQuery")
	procPdhAddEnglishCounterW        = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
)

// IID_IDXGIFactory1 {770aae78-f26f-4dba-a829-253c83d1b387}
var iidIDXGIFactory1 = windows.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba,
	Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}

// vtable slots (IUnknown 0-2, IDXGIObject 3-6, then the interface's own).
const (
	vtRelease         = 2
	vtEnumAdapters1   = 12 // IDXGIFactory1
	vtGetDesc1        = 10 // IDXGIAdapter1
	dxgiFlagSoftware  = 2
	integratedVRAMCap = 512 << 20 // below this, the GPU really lives in shared RAM
)

// dxgiAdapterDesc1 mirrors DXGI_ADAPTER_DESC1 (natural alignment matches C on amd64/arm64).
type dxgiAdapterDesc1 struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uintptr
	DedicatedSystemMemory uintptr
	SharedSystemMemory    uintptr
	LuidLow               uint32
	LuidHigh              int32
	Flags                 uint32
}

type dxgiAdapter struct {
	GPU
	luid      string
	dedicated uint64
	shared    uint64
}

// comCall invokes vtable slot idx of the COM object obj.
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := (*[32]uintptr)(vtbl)[idx]
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func enumDXGI() []dxgiAdapter {
	if procCreateDXGIFactory1.Find() != nil {
		return nil
	}
	var factory unsafe.Pointer
	hr, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory)))
	if int32(hr) < 0 || factory == nil {
		return nil
	}
	defer comCall(factory, vtRelease)

	var out []dxgiAdapter
	for i := uintptr(0); i < 16; i++ {
		var adapter unsafe.Pointer
		if hr := comCall(factory, vtEnumAdapters1, i, uintptr(unsafe.Pointer(&adapter))); int32(hr) < 0 || adapter == nil {
			break // DXGI_ERROR_NOT_FOUND: no more adapters
		}
		var d dxgiAdapterDesc1
		hr := comCall(adapter, vtGetDesc1, uintptr(unsafe.Pointer(&d)))
		comCall(adapter, vtRelease)
		if int32(hr) < 0 || d.Flags&dxgiFlagSoftware != 0 {
			continue // skip the WARP "Microsoft Basic Render Driver"
		}
		a := dxgiAdapter{
			GPU:       GPU{Name: windows.UTF16ToString(d.Description[:]), Vendor: vendorName(d.VendorID)},
			luid:      luidKey(uint32(d.LuidHigh), d.LuidLow),
			dedicated: uint64(d.DedicatedVideoMemory),
			shared:    uint64(d.SharedSystemMemory),
		}
		a.VRAMTotal = a.dedicated
		if a.dedicated < integratedVRAMCap {
			a.VRAMTotal = a.shared
			a.SharedMemory = true
		}
		out = append(out, a)
	}
	return out
}

// pdhQuery is a long-lived PDH query: rate counters such as GPU utilization
// are computed between two collections, so keeping it open makes every poll
// report usage since the previous one.
type pdhQuery struct {
	handle   uintptr
	engines  uintptr
	dedic    uintptr
	shared   uintptr
	openedAt time.Time
}

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
)

// pdhItem mirrors PDH_FMT_COUNTERVALUE_ITEM_W with a double value.
type pdhItem struct {
	Name    *uint16
	CStatus uint32
	Value   float64
}

func openPDH() *pdhQuery {
	if procPdhOpenQueryW.Find() != nil {
		return nil
	}
	q := &pdhQuery{openedAt: time.Now()}
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q.handle))); r != 0 {
		return nil
	}
	add := func(path string, dst *uintptr) {
		p, _ := windows.UTF16PtrFromString(path)
		if r, _, _ := procPdhAddEnglishCounterW.Call(q.handle, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(dst))); r != 0 {
			*dst = 0
		}
	}
	// English names: localized Windows (es-ES…) would not match otherwise.
	add(`\GPU Engine(*)\Utilization Percentage`, &q.engines)
	add(`\GPU Adapter Memory(*)\Dedicated Usage`, &q.dedic)
	add(`\GPU Adapter Memory(*)\Shared Usage`, &q.shared)
	if q.engines == 0 && q.dedic == 0 {
		q.close()
		return nil // pre-1709 Windows: no GPU counters
	}
	return q
}

func (q *pdhQuery) close() { procPdhCloseQuery.Call(q.handle) }

func (q *pdhQuery) collect() bool {
	r, _, _ := procPdhCollectQueryData.Call(q.handle)
	return r == 0
}

// array returns instance name -> value for a wildcard counter.
func (q *pdhQuery) array(counter uintptr) map[string]float64 {
	if counter == 0 {
		return nil
	}
	var size, count uint32
	r, _, _ := procPdhGetFormattedCounterArrayW.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != pdhMoreData || size == 0 {
		return nil
	}
	buf := make([]uint64, size/8+1) // uint64 for alignment; names point inside it
	r, _, _ = procPdhGetFormattedCounterArrayW.Call(counter, pdhFmtDouble|pdhFmtNoCap100,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil
	}
	items := unsafe.Slice((*pdhItem)(unsafe.Pointer(&buf[0])), count)
	out := make(map[string]float64, count)
	for _, it := range items {
		if it.CStatus > 1 { // only PDH_CSTATUS_VALID_DATA / NEW_DATA
			continue
		}
		out[windows.UTF16PtrToString(it.Name)] += it.Value
	}
	return out
}

var gpuState struct {
	mu       sync.Mutex
	adapters []dxgiAdapter
	listedAt time.Time
	pdh      *pdhQuery
}

func gpuStats(ctx context.Context) []GPU {
	s := &gpuState
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.adapters == nil || time.Since(s.listedAt) > time.Minute {
		s.adapters = enumDXGI()
		s.listedAt = time.Now()
	}
	// Reopen periodically so wildcard counters pick up new GPU processes.
	if s.pdh != nil && time.Since(s.pdh.openedAt) > 2*time.Minute {
		s.pdh.close()
		s.pdh = nil
	}
	if s.pdh == nil {
		if s.pdh = openPDH(); s.pdh != nil {
			s.pdh.collect() // first sample; rates need two
			time.Sleep(250 * time.Millisecond)
		}
	}

	var util map[string]float64
	dedic, shared := map[string]float64{}, map[string]float64{}
	if s.pdh != nil && s.pdh.collect() {
		util = gpuUtilization(s.pdh.array(s.pdh.engines))
		for name, v := range s.pdh.array(s.pdh.dedic) {
			if l, ok := luidFromInstance(name); ok {
				dedic[l] += v
			}
		}
		for name, v := range s.pdh.array(s.pdh.shared) {
			if l, ok := luidFromInstance(name); ok {
				shared[l] += v
			}
		}
	}

	out := make([]GPU, 0, len(s.adapters))
	hasNvidia := false
	for _, a := range s.adapters {
		g := a.GPU
		if v, ok := util[a.luid]; ok {
			g.UsagePercent = &v
		}
		if v, ok := dedic[a.luid]; ok {
			used := uint64(v)
			if g.SharedMemory {
				used += uint64(shared[a.luid])
			}
			g.VRAMUsed = &used
		}
		hasNvidia = hasNvidia || g.Vendor == "NVIDIA"
		out = append(out, g)
	}
	if hasNvidia {
		nvsmi := filepath.Join(os.Getenv("ProgramFiles"), "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe")
		mergeNvidia(out, nvidiaSMI(ctx, nvsmi))
	}
	return out
}

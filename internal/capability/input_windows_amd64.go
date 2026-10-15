package capability

import "unsafe"

// SendInput rejects every event unless cbSize == sizeof(INPUT), 40 bytes on
// amd64. These two lines stop the build if input32 drifts from that.
var (
	_ [40 - unsafe.Sizeof(input32{})]byte
	_ [unsafe.Sizeof(input32{}) - 40]byte
)

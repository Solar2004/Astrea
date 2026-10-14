package supervise

import (
	"bytes"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// MaxLogChunk caps one log read. The hub truncates streams at 64 KiB, so a
// single result stays comfortably below that.
const MaxLogChunk = 48 << 10

// LogChunk is a window of a log file.
type LogChunk struct {
	Data string `json:"data"`
	// Offset is where Data starts; NextOffset is where to continue reading.
	Offset     int64 `json:"offset"`
	NextOffset int64 `json:"next_offset"`
	Size       int64 `json:"size"`
	// More is true when there is unread data after NextOffset.
	More bool `json:"more"`
}

// ReadLog reads path. With tailLines > 0 it returns (at most) the last
// tailLines lines; otherwise up to maxBytes starting at offset.
func ReadLog(path string, offset int64, tailLines, maxBytes int) (LogChunk, error) {
	if maxBytes <= 0 || maxBytes > MaxLogChunk {
		maxBytes = MaxLogChunk
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return LogChunk{}, nil
	}
	if err != nil {
		return LogChunk{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return LogChunk{}, err
	}
	size := st.Size()

	if tailLines > 0 {
		offset = size - int64(maxBytes)
		if offset < 0 {
			offset = 0
		}
	}
	if offset < 0 || offset > size {
		offset = size
	}
	buf := make([]byte, min(int64(maxBytes), size-offset))
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return LogChunk{}, err
	}
	buf = buf[:n]

	if tailLines > 0 {
		// Keep whole lines only, and only the last tailLines of them.
		if offset > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				offset += int64(i + 1)
				buf = buf[i+1:]
			}
		}
		lines := strings.SplitAfter(string(buf), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > tailLines {
			skip := strings.Join(lines[:len(lines)-tailLines], "")
			offset += int64(len(skip))
			buf = buf[len(skip):]
		}
	} else {
		// Don't split a UTF-8 rune at the end of the window.
		i := len(buf) - 1
		for i > 0 && len(buf)-i < utf8.UTFMax && buf[i]&0xC0 == 0x80 {
			i--
		}
		if i >= 0 && buf[i] >= utf8.RuneSelf && !utf8.FullRune(buf[i:]) {
			buf = buf[:i]
		}
	}
	next := offset + int64(len(buf))
	return LogChunk{Data: string(buf), Offset: offset, NextOffset: next, Size: size, More: next < size}, nil
}

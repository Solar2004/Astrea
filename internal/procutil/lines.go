package procutil

import (
	"bytes"
	"os/exec"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// PipeGrace is how long Wait keeps reading a command's output after the
// command itself exited. Background children (`ollama serve &`, `start x`)
// inherit stdout and can hold it open forever; past this grace the agent
// stops waiting for them instead of hanging the call.
const PipeGrace = 2 * time.Second

// StreamOutput sends cmd's stdout and stderr line by line to emit, and sets
// WaitDelay so cmd.Wait returns PipeGrace after the process exits even if a
// detached child still holds the pipes.
func StreamOutput(cmd *exec.Cmd, emit func(protocol.StreamChunk)) (flush func()) {
	out := &lineWriter{channel: "stdout", emit: emit}
	errw := &lineWriter{channel: "stderr", emit: emit}
	cmd.Stdout, cmd.Stderr = out, errw
	cmd.WaitDelay = PipeGrace
	return func() { out.flush(); errw.flush() }
}

// lineWriter emits each complete line as a stream chunk.
type lineWriter struct {
	mu      sync.Mutex
	channel string
	emit    func(protocol.StreamChunk)
	buf     []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(protocol.StreamChunk{Channel: w.channel, Data: string(w.buf[:i+1])})
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 { // very long line without newline
		w.emit(protocol.StreamChunk{Channel: w.channel, Data: string(w.buf)})
		w.buf = nil
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(protocol.StreamChunk{Channel: w.channel, Data: string(w.buf) + "\n"})
		w.buf = nil
	}
}

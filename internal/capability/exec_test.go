package capability

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// A server started in the background inherits stdout; exec must return when
// the shell exits instead of waiting for that child.
func TestExecBackgroundChildDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	var mu sync.Mutex
	var out strings.Builder
	start := time.Now()
	res, err := Exec{}.Invoke(context.Background(), json.RawMessage(`{"command":"sleep 20 & echo started"}`), func(c protocol.StreamChunk) {
		mu.Lock()
		out.WriteString(c.Data)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("exec blocked %s waiting for a background child", d)
	}
	if out.String() != "started\n" || res.(execResult).ExitCode != 0 {
		t.Fatalf("output %q, result %+v", out.String(), res)
	}
}

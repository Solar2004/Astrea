package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// fakeCap is a capability whose behavior is supplied by the test.
type fakeCap struct {
	name string
	fn   func(ctx context.Context, emit capability.Emit) (any, error)
}

func (f fakeCap) Name() string                    { return f.name }
func (f fakeCap) Describe() capability.Descriptor { return capability.Descriptor{Name: f.name} }
func (f fakeCap) Invoke(ctx context.Context, _ json.RawMessage, emit capability.Emit) (any, error) {
	return f.fn(ctx, emit)
}

func startAgent(t *testing.T, caps ...capability.Capability) (wsURL string, close func()) {
	t.Helper()
	reg := capability.NewRegistry()
	for _, c := range caps {
		reg.Register(c)
	}
	srv := httptest.NewServer((&Server{Registry: reg, Auth: auth.New("tok")}).Handler())
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", srv.Close
}

func TestCallResultAndStreams(t *testing.T) {
	url, stop := startAgent(t, fakeCap{"echo", func(_ context.Context, emit capability.Emit) (any, error) {
		emit(protocol.StreamChunk{Channel: "stdout", Data: "hi\n"})
		return map[string]int{"n": 1}, nil
	}})
	defer stop()
	c, err := Dial(context.Background(), url, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Call(context.Background(), "echo", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Result) != `{"n":1}` || len(res.Streams) != 1 || res.Streams[0].Data != "hi\n" {
		t.Fatalf("unexpected result: %s %+v", res.Result, res.Streams)
	}
}

func TestCancelPropagatesToAgent(t *testing.T) {
	cancelled := make(chan struct{})
	url, stop := startAgent(t, fakeCap{"block", func(ctx context.Context, _ capability.Emit) (any, error) {
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}})
	defer stop()
	c, err := Dial(context.Background(), url, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "block", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("agent-side capability was not cancelled")
	}
}

func TestConnLostIsNotRetried(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 4)
	url, stop := startAgent(t, fakeCap{"once", func(ctx context.Context, _ capability.Emit) (any, error) {
		runs.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	defer stop()
	rc := NewReconnecting(url, "tok")
	go func() {
		<-started
		rc.mu.Lock()
		rc.cur.ws.CloseNow() // drop the connection mid-call
		rc.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rc.Call(ctx, "once", nil); !errors.Is(err, ErrConnLost) {
		t.Fatalf("want ErrConnLost, got %v", err)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("capability ran %d times; a lost connection must not be retried", n)
	}
}

func TestStreamOutputIsBounded(t *testing.T) {
	line := strings.Repeat("x", 1023) + "\n"
	url, stop := startAgent(t, fakeCap{"flood", func(_ context.Context, emit capability.Emit) (any, error) {
		for i := 0; i < 1000; i++ { // ~1 MiB
			emit(protocol.StreamChunk{Channel: "stdout", Data: line})
		}
		return nil, nil
	}})
	defer stop()
	c, err := Dial(context.Background(), url, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Call(context.Background(), "flood", nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, s := range res.Streams {
		total += len(s.Data)
	}
	if total > MaxStreamBytes+2048 || res.OmittedBytes == 0 {
		t.Fatalf("kept %d bytes, omitted %d; want bounded output", total, res.OmittedBytes)
	}
}

func TestBadTokenRejected(t *testing.T) {
	url, stop := startAgent(t)
	defer stop()
	if _, err := Dial(context.Background(), url, "wrong"); err == nil {
		t.Fatal("dial with a wrong token should fail")
	}
}

// Requests over the library's 32 KiB default (fs.write of a binary, a big
// recipe) used to drop the connection.
func TestLargeRequest(t *testing.T) {
	url, stop := startAgent(t, fakeCap{"big", func(context.Context, capability.Emit) (any, error) {
		return "ok", nil
	}})
	defer stop()
	c, err := Dial(context.Background(), url, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.Call(context.Background(), "big", map[string]any{"data": strings.Repeat("x", 3<<20)})
	if err != nil || string(res.Result) != `"ok"` {
		t.Fatalf("got %v, %v", res, err)
	}
}

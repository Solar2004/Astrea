package guibridge

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/transport"
)

// echo is a stand-in GUI capability that streams a line and returns its input.
type echo struct{}

func (echo) Name() string { return "screen.capture" }
func (echo) Describe() capability.Descriptor {
	return capability.Descriptor{Name: "screen.capture", Description: "fake", InputSchema: json.RawMessage(`{}`)}
}
func (echo) Invoke(_ context.Context, p json.RawMessage, emit capability.Emit) (any, error) {
	emit(protocol.StreamChunk{Channel: "stdout", Data: "hola\n"})
	return map[string]any{"got": json.RawMessage(p)}, nil
}

// helper starts a fake GUI helper on addr and returns a stop function.
func helper(t *testing.T, addr string) func() {
	t.Helper()
	reg := capability.NewRegistry()
	reg.Register(echo{})
	srv := &http.Server{Handler: (&transport.Server{Registry: reg, Auth: auth.New("tok")}).Handler()}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may linger briefly after a restart
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	tl := &trackingListener{Listener: ln}
	go srv.Serve(tl)
	// Like the helper process dying at logoff: close hijacked WebSockets too,
	// which http.Server.Close leaves open.
	return func() { srv.Close(); tl.closeAll() }
}

type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackingListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestRemoteWithoutHelperFailsFast(t *testing.T) {
	b := New("ws://"+freeAddr(t)+"/ws", "tok")
	r := Remote{Bridge: b, Local: echo{}}
	start := time.Now()
	_, err := r.Invoke(context.Background(), nil, func(protocol.StreamChunk) {})
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeUnavailable || !strings.Contains(pe.Message, "no interactive session") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > dialTimeout+time.Second {
		t.Fatalf("took %s", time.Since(start))
	}

	r.Fallback = func() (any, error) { return "fallback", nil }
	if res, err := r.Invoke(context.Background(), nil, func(protocol.StreamChunk) {}); err != nil || res != "fallback" {
		t.Fatalf("fallback: %v %v", res, err)
	}
}

func TestRemoteForwardsAndSurvivesHelperRestart(t *testing.T) {
	addr := freeAddr(t)
	stop := helper(t, addr)
	b := New("ws://"+addr+"/ws", "tok")
	r := Remote{Bridge: b, Local: echo{}, As: "gui.capture"}
	if r.Describe().Name != "gui.capture" || r.Name() != "gui.capture" {
		t.Fatal("rename not applied")
	}

	var streamed []string
	emit := func(c protocol.StreamChunk) { streamed = append(streamed, c.Data) }
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"x":1}`), emit)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := res.(json.RawMessage); !strings.Contains(string(raw), `"x":1`) || len(streamed) != 1 {
		t.Fatalf("res=%s streamed=%q", raw, streamed)
	}

	// Logoff: helper gone → unavailable.
	stop()
	time.Sleep(100 * time.Millisecond)
	if _, err := r.Invoke(context.Background(), nil, emit); !errors.Is(err, NoSession) {
		t.Fatalf("after helper stop: %v", err)
	}

	// Logon again: the bridge reconnects by itself.
	stop = helper(t, addr)
	defer stop()
	if _, err := r.Invoke(context.Background(), nil, emit); err != nil {
		t.Fatalf("after helper restart: %v", err)
	}
}

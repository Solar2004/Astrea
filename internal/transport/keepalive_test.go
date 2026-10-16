package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// An agent that stops answering (the PC went to standby, the network dropped
// the connection) must not leave the hub holding a dead connection.
func TestKeepaliveClosesDeadConnection(t *testing.T) {
	pingEvery, pingTimeout = 50*time.Millisecond, 100*time.Millisecond
	defer func() { pingEvery, pingTimeout = 15*time.Second, 8*time.Second }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		// Read (and so answer pings) for a while, then go silent.
		ctx, cancel := context.WithTimeout(r.Context(), 300*time.Millisecond)
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				break
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(250 * time.Millisecond)
	if !c.Alive() {
		t.Fatal("closed while the agent was still answering pings")
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Alive() {
		if time.Now().After(deadline) {
			t.Fatal("dead connection still reported alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

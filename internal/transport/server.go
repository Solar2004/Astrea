// Package transport implements the agent's control-channel server: a WebSocket
// endpoint that accepts protocol.Frame requests and dispatches them to the
// capability registry, streaming output and the terminal result back.
package transport

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/Solar2004/Astrea/internal/auth"
	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/security"
	"github.com/coder/websocket"
)

// Server serves the control channel and advertises the capability set.
type Server struct {
	Registry *capability.Registry
	Auth     *auth.Token
	Guard    *security.Guard
	Audit    *security.Audit
	// Dyn, if set, serves dynamic endpoints mounted under /dyn/.
	Dyn    http.Handler
	Logger *log.Logger
}

// Handler returns the HTTP handler mounting the control and metadata routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/capabilities", s.handleCapabilities)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	if s.Dyn != nil {
		// Dynamic endpoints are reachable over the tailnet without the control
		// token so Hermes-built APIs can be consumed by local tools.
		mux.Handle("/dyn/", s.Dyn)
	}
	return mux
}

// handleCapabilities returns the advertised descriptors (used by the hub to
// build its MCP tool list).
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.Check(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Registry.Descriptors())
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.Auth.Check(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	// No read deadline: this is a long-lived control channel.
	conn := &conn{ws: c, srv: s, ctx: r.Context(), inflight: map[string]context.CancelFunc{}}
	conn.serve()
}

// conn wraps a single accepted control connection and serializes writes.
type conn struct {
	ws      *websocket.Conn
	srv     *Server
	ctx     context.Context
	writeMu sync.Mutex

	mu       sync.Mutex
	inflight map[string]context.CancelFunc // request ID -> cancel
}

func (c *conn) serve() {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case protocol.FrameRequest:
			// Each request runs in its own goroutine so long/streaming calls
			// do not block the read loop for other requests.
			ctx, cancel := context.WithCancel(c.ctx)
			c.mu.Lock()
			c.inflight[f.ID] = cancel
			c.mu.Unlock()
			go func() {
				defer func() {
					c.mu.Lock()
					delete(c.inflight, f.ID)
					c.mu.Unlock()
					cancel()
				}()
				c.handle(ctx, f)
			}()
		case protocol.FrameCancel:
			c.mu.Lock()
			cancel := c.inflight[f.ID]
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}
}

func (c *conn) handle(ctx context.Context, f protocol.Frame) {
	emit := func(chunk protocol.StreamChunk) {
		c.send(protocol.Frame{Type: protocol.FrameStream, ID: f.ID, Stream: &chunk})
	}
	// Authorization guard (allow/deny lists + kill switch) runs before dispatch.
	if c.srv.Guard != nil {
		if err := c.srv.Guard.Check(f.Method); err != nil {
			c.srv.Audit.Log(f.Method, f.Payload, err)
			c.send(protocol.Frame{Type: protocol.FrameError, ID: f.ID, Error: err.(*protocol.Error)})
			return
		}
	}
	result, err := c.srv.Registry.Dispatch(ctx, f.Method, f.Payload, emit)
	c.srv.Audit.Log(f.Method, f.Payload, err)
	if err != nil {
		pe, ok := err.(*protocol.Error)
		if !ok {
			pe = &protocol.Error{Code: protocol.CodeInternal, Message: err.Error()}
		}
		c.send(protocol.Frame{Type: protocol.FrameError, ID: f.ID, Error: pe})
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		c.send(protocol.Frame{Type: protocol.FrameError, ID: f.ID, Error: &protocol.Error{Code: protocol.CodeInternal, Message: err.Error()}})
		return
	}
	c.send(protocol.Frame{Type: protocol.FrameResult, ID: f.ID, Result: raw})
}

func (c *conn) send(f protocol.Frame) {
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.Write(c.ctx, websocket.MessageText, data); err != nil && c.srv.Logger != nil {
		c.srv.Logger.Printf("write: %v", err)
	}
}

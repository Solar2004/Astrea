package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// ErrNotSent wraps failures that happened before the request reached the
// agent. Only these are safe to retry: anything later may already have run.
var ErrNotSent = errors.New("request not sent")

// ErrConnLost reports that the connection dropped while waiting for the
// response. The capability may or may not have run on the agent.
var ErrConnLost = errors.New("connection lost while waiting for the agent (the call may or may not have run)")

// MaxStreamBytes bounds the streamed output kept per call. Past it the head
// and tail are kept and the middle is dropped, so a runaway command cannot
// exhaust the hub's memory or the model's context window.
const MaxStreamBytes = 64 << 10

// Client is the hub's connection to a single agent's control channel. It
// correlates responses to requests by ID and buffers stream chunks per call.
type Client struct {
	url   string
	token string

	mu      sync.Mutex
	ws      *websocket.Conn
	pending map[string]*call
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

// call tracks one in-flight request. Its fields are written only by the read
// loop before done is closed, and read by Call only after.
type call struct {
	out    streamBuf
	done   chan struct{}
	result json.RawMessage
	err    error
}

// Dial connects to the agent at wsURL (e.g. ws://100.x.y.z:7700/ws).
func Dial(ctx context.Context, wsURL, token string) (*Client, error) {
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	ws, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(64 << 20) // allow large results (e.g. file reads)
	cctx, cancel := context.WithCancel(context.Background())
	c := &Client{url: wsURL, token: token, ws: ws, pending: map[string]*call{}, ctx: cctx, cancel: cancel}
	go c.readLoop()
	go c.keepalive()
	return c, nil
}

// Keepalive timing: a connection the network silently dropped (NAT timeout,
// Tailscale switching paths, the PC going to standby) still looks open and
// every call on it just times out until the OS gives up on TCP, which takes
// ~15 minutes. A ping that goes unanswered closes it so the next call redials.
var (
	pingEvery   = 15 * time.Second
	pingTimeout = 8 * time.Second
)

func (c *Client) keepalive() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.probe()
		}
	}
}

// probe pings the agent and closes the connection if it does not answer.
func (c *Client) probe() {
	ctx, cancel := context.WithTimeout(c.ctx, pingTimeout)
	defer cancel()
	if err := c.ws.Ping(ctx); err != nil && c.ctx.Err() == nil {
		c.Close()
	}
}

// Close terminates the connection.
func (c *Client) Close() {
	c.cancel()
	c.ws.CloseNow()
}

// Alive reports whether the read loop is still running.
func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.failAll()
			return
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		c.mu.Lock()
		cl := c.pending[f.ID]
		switch f.Type {
		case protocol.FrameResult, protocol.FrameError:
			// Remove under the lock so failAll can never close done twice.
			delete(c.pending, f.ID)
		}
		c.mu.Unlock()
		if cl == nil {
			continue
		}
		switch f.Type {
		case protocol.FrameStream:
			if f.Stream != nil {
				cl.out.add(*f.Stream)
			}
		case protocol.FrameResult:
			cl.result = f.Result
			close(cl.done)
		case protocol.FrameError:
			if f.Error != nil {
				cl.err = f.Error
			} else {
				cl.err = &protocol.Error{Code: protocol.CodeInternal, Message: "agent returned an empty error"}
			}
			close(cl.done)
		}
	}
}

func (c *Client) failAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, cl := range c.pending {
		cl.err = ErrConnLost
		close(cl.done)
		delete(c.pending, id)
	}
}

// CallResult is the aggregated outcome of a capability invocation.
type CallResult struct {
	Result  json.RawMessage
	Streams []protocol.StreamChunk
	// OmittedBytes counts streamed output dropped to respect MaxStreamBytes.
	OmittedBytes int
}

// Call invokes a capability and blocks until the terminal result or error.
// Stream chunks are aggregated and returned alongside the result. If ctx ends
// first, the agent is told to cancel the request.
func (c *Client) Call(ctx context.Context, method string, payload any) (*CallResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	cl := &call{done: make(chan struct{})}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: connection closed", ErrNotSent)
	}
	c.pending[id] = cl
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	frame := protocol.Frame{Type: protocol.FrameRequest, ID: id, Method: method, Payload: raw, Work: protocol.WorkSpec(ctx)}
	data, _ := json.Marshal(frame)
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		forget()
		return nil, fmt.Errorf("%w: %v", ErrNotSent, err)
	}

	select {
	case <-ctx.Done():
		forget()
		c.sendCancel(id)
		go c.probe() // a call that timed out may mean a dead connection
		return nil, ctx.Err()
	case <-cl.done:
	}
	if cl.err != nil {
		return nil, cl.err
	}
	streams, omitted := cl.out.chunks()
	return &CallResult{Result: cl.result, Streams: streams, OmittedBytes: omitted}, nil
}

// sendCancel best-effort tells the agent to abort request id.
func (c *Client) sendCancel(id string) {
	data, _ := json.Marshal(protocol.Frame{Type: protocol.FrameCancel, ID: id})
	_ = c.ws.Write(c.ctx, websocket.MessageText, data)
}

// streamBuf keeps the first and last MaxStreamBytes/2 of streamed output.
type streamBuf struct {
	head, tail         []protocol.StreamChunk
	headBytes, tailLen int
	omitted            int
}

func (b *streamBuf) add(ch protocol.StreamChunk) {
	const half = MaxStreamBytes / 2
	if b.headBytes < half && len(b.tail) == 0 {
		b.head = append(b.head, ch)
		b.headBytes += len(ch.Data)
		return
	}
	b.tail = append(b.tail, ch)
	b.tailLen += len(ch.Data)
	for len(b.tail) > 1 && b.tailLen > half {
		b.omitted += len(b.tail[0].Data)
		b.tailLen -= len(b.tail[0].Data)
		b.tail = b.tail[1:]
	}
}

func (b *streamBuf) chunks() ([]protocol.StreamChunk, int) {
	if b.omitted == 0 {
		return append(b.head, b.tail...), 0
	}
	out := append([]protocol.StreamChunk{}, b.head...)
	out = append(out, protocol.StreamChunk{Channel: "astrea", Data: fmt.Sprintf("\n… [%d bytes of output omitted] …\n", b.omitted)})
	return append(out, b.tail...), b.omitted
}

// CapDescriptor mirrors capability.Descriptor for the hub side.
type CapDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// FetchCapabilities fetches the agent's advertised descriptors over HTTP. It
// does not require a live WebSocket connection, so the hub can discover tools
// (or fail fast) independently of the control channel.
func FetchCapabilities(ctx context.Context, httpURL, token string) ([]CapDescriptor, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpURL, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("capabilities: %s", resp.Status)
	}
	var out []CapDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

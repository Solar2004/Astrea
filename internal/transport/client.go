package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Client is the hub's connection to a single agent's control channel. It
// correlates responses to requests by ID and buffers stream chunks per call.
type Client struct {
	url   string
	token string

	mu      sync.Mutex
	ws      *websocket.Conn
	pending map[string]*call
	ctx     context.Context
	cancel  context.CancelFunc
}

// call tracks one in-flight request.
type call struct {
	streams []protocol.StreamChunk
	done    chan struct{}
	result  json.RawMessage
	err     *protocol.Error
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
	return c, nil
}

// Close terminates the connection.
func (c *Client) Close() {
	c.cancel()
	c.ws.CloseNow()
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.failAll(err)
			return
		}
		var f protocol.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		c.mu.Lock()
		cl := c.pending[f.ID]
		c.mu.Unlock()
		if cl == nil {
			continue
		}
		switch f.Type {
		case protocol.FrameStream:
			if f.Stream != nil {
				cl.streams = append(cl.streams, *f.Stream)
			}
		case protocol.FrameResult:
			cl.result = f.Result
			close(cl.done)
		case protocol.FrameError:
			cl.err = f.Error
			close(cl.done)
		}
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cl := range c.pending {
		cl.err = &protocol.Error{Code: protocol.CodeInternal, Message: "connection lost: " + err.Error()}
		close(cl.done)
		delete(c.pending, id)
	}
}

// CallResult is the aggregated outcome of a capability invocation.
type CallResult struct {
	Result  json.RawMessage
	Streams []protocol.StreamChunk
}

// Call invokes a capability and blocks until the terminal result or error.
// Stream chunks are aggregated and returned alongside the result.
func (c *Client) Call(ctx context.Context, method string, payload any) (*CallResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	cl := &call{done: make(chan struct{})}
	c.mu.Lock()
	c.pending[id] = cl
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	frame := protocol.Frame{Type: protocol.FrameRequest, ID: id, Method: method, Payload: raw}
	data, _ := json.Marshal(frame)
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-cl.done:
	}
	if cl.err != nil {
		return nil, fmt.Errorf("%s: %s", cl.err.Code, cl.err.Message)
	}
	return &CallResult{Result: cl.result, Streams: cl.streams}, nil
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
	var out []CapDescriptor
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

package transport

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ReconnectingClient wraps the dialing of a single agent so the hub survives
// the agent rebooting or the network dropping. It (re)dials lazily: the first
// Call — or the first after a dropped connection — establishes a fresh
// connection with bounded backoff.
type ReconnectingClient struct {
	wsURL string
	token string

	mu  sync.Mutex
	cur *Client
}

// NewReconnecting returns a client that connects on demand to wsURL.
func NewReconnecting(wsURL, token string) *ReconnectingClient {
	return &ReconnectingClient{wsURL: wsURL, token: token}
}

// connect returns a live client, dialing if necessary with backoff until ctx
// expires.
func (rc *ReconnectingClient) connect(ctx context.Context) (*Client, error) {
	rc.mu.Lock()
	if rc.cur != nil && rc.cur.Alive() {
		c := rc.cur
		rc.mu.Unlock()
		return c, nil
	}
	if rc.cur != nil {
		rc.cur.Close()
		rc.cur = nil
	}
	rc.mu.Unlock()

	backoff := 250 * time.Millisecond
	for {
		c, err := Dial(ctx, rc.wsURL, rc.token)
		if err == nil {
			rc.mu.Lock()
			rc.cur = c
			rc.mu.Unlock()
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// drop discards the current connection so the next Call redials.
func (rc *ReconnectingClient) drop(c *Client) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.cur == c {
		rc.cur.Close()
		rc.cur = nil
	}
}

// Call invokes a capability. It redials and retries once only when the request
// never reached the agent; a connection lost mid-call is reported, not retried,
// because the capability (exec, fs.write, proc.kill…) may already have run.
func (rc *ReconnectingClient) Call(ctx context.Context, method string, payload any) (*CallResult, error) {
	for attempt := 0; ; attempt++ {
		c, err := rc.connect(ctx)
		if err != nil {
			return nil, err
		}
		res, err := c.Call(ctx, method, payload)
		switch {
		case errors.Is(err, ErrNotSent) && attempt == 0:
			rc.drop(c)
			continue
		case errors.Is(err, ErrNotSent), errors.Is(err, ErrConnLost):
			rc.drop(c)
		}
		return res, err
	}
}

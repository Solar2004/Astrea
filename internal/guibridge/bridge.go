// Package guibridge lets a headless agent (started at boot, before anyone logs
// in) serve the capabilities that need an interactive desktop — screen,
// mouse, keyboard, idle detection — by forwarding them to a GUI helper: the
// same binary running with --role gui inside the user's session, listening on
// loopback only.
//
// The hub keeps seeing ONE machine: the headless agent advertises the GUI
// capabilities with their usual descriptors and answers "unavailable" when no
// helper is reachable (nobody logged in), instead of hanging.
package guibridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/capability"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/transport"
)

// dialTimeout bounds how long a call waits for the helper before reporting
// that there is no interactive session. It is local loopback, so a live
// helper answers in milliseconds.
const dialTimeout = 2 * time.Second

// NoSession is the error returned when the GUI helper is not reachable.
var NoSession = &protocol.Error{
	Code: protocol.CodeUnavailable,
	Message: "no interactive session: nobody is logged in on this PC, so screen, mouse and keyboard are unavailable. " +
		"Log in on the machine (or enable autologon) and retry; everything else (exec, services, downloads…) keeps working.",
}

// helperDown is the error returned when the helper is not reachable but
// someone IS signed in: the helper died (e.g. its restart after an update was
// blocked), so "nobody is logged in" would be a lie.
func helperDown(user string) error {
	return &protocol.Error{
		Code: protocol.CodeUnavailable,
		Message: fmt.Sprintf("the GUI helper (screen, mouse, keyboard) is not running although %s is signed in on this PC; "+
			"the agent starts it again by itself within a minute, retry then. Everything else (exec, services, downloads…) keeps working.", user),
	}
}

// Bridge is a lazily (re)connected client to the GUI helper.
type Bridge struct {
	wsURL string
	token string

	// SignedIn, if set, names the user signed in at the console ("" if
	// nobody). It tells "nobody logged in" from "the helper is down".
	SignedIn func() string

	mu  sync.Mutex
	cur *transport.Client
}

// New returns a bridge to the helper's control channel, e.g.
// ws://127.0.0.1:7701/ws.
func New(wsURL, token string) *Bridge { return &Bridge{wsURL: wsURL, token: token} }

// SetToken changes the bearer the bridge uses to reach the helper and drops
// the cached connection so the next call reconnects with it. The headless
// agent calls this when it hands the session over between two helpers that
// authenticate differently: the administrator helper (started by the logon
// task, reads config.json, uses cfg.Token) and the standard-user helper
// (launched by the headless agent itself with a random per-launch secret).
func (b *Bridge) SetToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if token == b.token {
		return
	}
	b.token = token
	if b.cur != nil {
		b.cur.Close()
		b.cur = nil
	}
}

func (b *Bridge) client(ctx context.Context) (*transport.Client, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur != nil && b.cur.Alive() {
		return b.cur, nil
	}
	if b.cur != nil {
		b.cur.Close()
		b.cur = nil
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	c, err := transport.Dial(dctx, b.wsURL, b.token)
	if err != nil {
		return nil, err
	}
	b.cur = c
	return c, nil
}

func (b *Bridge) drop(c *transport.Client) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == c {
		b.cur.Close()
		b.cur = nil
	}
}

// Available reports whether a GUI helper answers right now.
func (b *Bridge) Available(ctx context.Context) bool {
	return b.Probe(ctx) == nil
}

// Probe is Available with the reason it failed (refused, timeout, auth…).
func (b *Bridge) Probe(ctx context.Context) error {
	_, err := b.client(ctx)
	return err
}

// Call forwards method to the helper. A request that never reached the
// helper (stale connection after a logoff/logon) is retried once on a fresh
// connection; one lost mid-call is not, since it may already have acted.
func (b *Bridge) Call(ctx context.Context, method string, payload json.RawMessage) (*transport.CallResult, error) {
	for attempt := 0; ; attempt++ {
		c, err := b.client(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, b.unreachable()
		}
		res, err := c.Call(ctx, method, payload)
		switch {
		case errors.Is(err, transport.ErrNotSent) && attempt == 0:
			b.drop(c)
			continue
		case errors.Is(err, transport.ErrNotSent), errors.Is(err, transport.ErrConnLost):
			b.drop(c)
			if errors.Is(err, transport.ErrNotSent) {
				return nil, b.unreachable()
			}
		}
		return res, err
	}
}

// unreachable is the error for a helper that does not answer: NoSession,
// unless someone is signed in, in which case the helper itself is down.
func (b *Bridge) unreachable() error {
	if b.SignedIn != nil {
		if u := b.SignedIn(); u != "" {
			return helperDown(u)
		}
	}
	return NoSession
}

// Remote advertises a capability locally but runs it in the GUI helper.
type Remote struct {
	Bridge *Bridge
	// Local provides the descriptor (and the method name unless Method is set).
	Local capability.Capability
	// As renames the capability on this agent (e.g. "gui.exec"); Method is
	// the name invoked on the helper. Both default to Local.Name().
	As, Method, Description string
	// Fallback, if set, answers instead of NoSession when no helper is up.
	Fallback func() (any, error)
}

func (r Remote) Name() string {
	if r.As != "" {
		return r.As
	}
	return r.Local.Name()
}

func (r Remote) Describe() capability.Descriptor {
	d := r.Local.Describe()
	d.Name = r.Name()
	if r.Description != "" {
		d.Description = r.Description
	}
	return d
}

func (r Remote) Invoke(ctx context.Context, payload json.RawMessage, emit capability.Emit) (any, error) {
	method := r.Method
	if method == "" {
		method = r.Local.Name()
	}
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	res, err := r.Bridge.Call(ctx, method, payload)
	if errors.Is(err, NoSession) && r.Fallback != nil {
		return r.Fallback()
	}
	if err != nil {
		return nil, err
	}
	for _, ch := range res.Streams {
		emit(ch)
	}
	if res.OmittedBytes > 0 {
		emit(protocol.StreamChunk{Channel: "stderr", Data: "[astrea: output truncated between the GUI helper and the agent]\n"})
	}
	return res.Result, nil
}

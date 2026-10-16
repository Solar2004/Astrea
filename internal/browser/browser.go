// Package browser drives a real Chromium/Chrome/Edge on the controlled PC
// through the Chrome DevTools Protocol (CDP). The AI navigates, reads the page
// as text (not pixels, so far fewer tokens), fills fields, clicks and runs JS.
//
// It speaks CDP directly over the coder/websocket already used elsewhere, so
// the agent needs no heavy browser-automation dependency: just a browser
// binary, which the aula PCs already have. Astrea launches its own instance
// with a separate profile and a debugging port, so it never fights the
// person's own windows.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Browser is a running headful browser Astrea controls. One per agent.
type Browser struct {
	// DataDir is where the dedicated profile lives.
	DataDir string
	// Port is the CDP port (0 = pick 9333).
	Port int
	// Headless runs with no visible window (default: visible, so the owner and
	// the live view can see what the AI does).
	Headless bool

	mu     sync.Mutex
	cmd    *exec.Cmd
	conn   *conn
	target string // current page target id
}

func (b *Browser) port() int {
	if b.Port != 0 {
		return b.Port
	}
	return 9333
}

// ensure makes sure a browser is up and we have a page target connected.
func (b *Browser) ensure(ctx context.Context) (*conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && b.conn.alive() {
		return b.conn, nil
	}
	if b.cmd == nil || b.cmd.ProcessState != nil {
		if err := b.launch(ctx); err != nil {
			return nil, err
		}
	}
	ws, target, err := b.pageWS(ctx)
	if err != nil {
		return nil, err
	}
	c, err := dial(ctx, ws)
	if err != nil {
		return nil, err
	}
	b.conn, b.target = c, target
	_, _ = c.call(ctx, "Page.enable", nil)
	_, _ = c.call(ctx, "Runtime.enable", nil)
	return c, nil
}

func (b *Browser) launch(ctx context.Context) error {
	exe := findBrowser()
	if exe == "" {
		return fmt.Errorf("no Chrome/Chromium/Edge found on this PC")
	}
	profile := filepath.Join(b.DataDir, "browser-profile")
	_ = os.MkdirAll(profile, 0o755)
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", b.port()),
		"--remote-allow-origins=*",
		"--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check",
		"--disable-popup-blocking", "--start-maximized",
		"about:blank",
	}
	if b.Headless {
		args = append([]string{"--headless=new"}, args...)
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch %s: %w", filepath.Base(exe), err)
	}
	b.cmd = cmd
	go cmd.Wait() // reap on exit
	// Wait for the CDP endpoint to answer.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := b.httpGet(ctx, "/json/version"); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("browser did not open its debugging port %d in time", b.port())
}

func (b *Browser) httpGet(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", b.port(), path)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// pageWS returns the WebSocket URL of a page target, creating one if needed.
func (b *Browser) pageWS(ctx context.Context) (ws, target string, err error) {
	body, err := b.httpGet(ctx, "/json/list")
	if err != nil {
		return "", "", err
	}
	var targets []cdpTarget
	_ = json.Unmarshal(body, &targets)
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, t.ID, nil
		}
	}
	// No page: open one.
	if _, err := b.httpGet(ctx, "/json/new?about:blank"); err != nil {
		return "", "", err
	}
	body, _ = b.httpGet(ctx, "/json/list")
	_ = json.Unmarshal(body, &targets)
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, t.ID, nil
		}
	}
	return "", "", fmt.Errorf("no page target available")
}

// Close shuts the browser down.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		b.conn.close()
		b.conn = nil
	}
	if b.cmd != nil && b.cmd.Process != nil && b.cmd.ProcessState == nil {
		_ = b.cmd.Process.Kill()
	}
	b.cmd = nil
}

// findBrowser locates a Chromium-family browser.
func findBrowser() string {
	if env := os.Getenv("ASTREA_BROWSER"); env != "" {
		return env
	}
	var names []string
	var dirs []string
	if runtime.GOOS == "windows" {
		names = []string{
			`Google\Chrome\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`Chromium\Application\chrome.exe`,
		}
		dirs = []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
		for _, d := range dirs {
			for _, n := range names {
				if d == "" {
					continue
				}
				p := filepath.Join(d, n)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
		return ""
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// --- CDP connection ---

type conn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	nextID int64
	dead   atomic.Bool

	mu      sync.Mutex
	pending map[int64]chan rpcResult
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

func dial(ctx context.Context, url string) (*conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to browser: %w", err)
	}
	ws.SetReadLimit(32 << 20) // screenshots arrive base64 over the socket
	cctx, ccancel := context.WithCancel(context.Background())
	c := &conn{ws: ws, ctx: cctx, cancel: ccancel, pending: map[int64]chan rpcResult{}}
	go c.readLoop()
	return c, nil
}

func (c *conn) alive() bool { return !c.dead.Load() }

func (c *conn) close() {
	c.dead.Store(true)
	c.cancel()
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

func (c *conn) readLoop() {
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.dead.Store(true)
			c.failAll(err)
			return
		}
		var msg struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
			continue // an event, not a reply
		}
		c.mu.Lock()
		ch := c.pending[msg.ID]
		delete(c.pending, msg.ID)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- rpcResult{err: fmt.Errorf("%s", msg.Error.Message)}
		} else {
			ch <- rpcResult{result: msg.Result}
		}
	}
}

func (c *conn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- rpcResult{err: err}
		delete(c.pending, id)
	}
}

func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	payload := map[string]any{"id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ch := make(chan rpcResult, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.result, r.err
	}
}

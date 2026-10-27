// Package download fetches large files (model weights of many GB) directly on
// the controlled machine: in the background, resumable after a dropped
// connection or an agent restart (via a .part file and HTTP Range), with
// optional sha256 verification and an up-front free-space check.
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v4/disk"
)

// States.
const (
	Running   = "running"
	Verifying = "verifying"
	Done      = "done"
	Failed    = "failed"
	Canceled  = "canceled"
)

// Request describes one download.
type Request struct {
	// URL is http(s), or hf://<owner>/<repo>/<path>[@<revision>] for a
	// HuggingFace file.
	URL string `json:"url"`
	// Dest is the target file, or an existing directory (or a path ending in
	// a separator) to keep the server's file name.
	Dest      string            `json:"dest"`
	SHA256    string            `json:"sha256,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Overwrite bool              `json:"overwrite,omitempty"`
	// Attempts bounds the tries on transient errors (0 = 5). A caller with
	// other sources to fall back on uses fewer, to move on sooner.
	Attempts int `json:"attempts,omitempty"`
}

// Status is a download's progress.
type Status struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Dest       string    `json:"dest"`
	State      string    `json:"state"`
	Bytes      int64     `json:"bytes"`
	Total      int64     `json:"total,omitempty"` // 0 = unknown
	Percent    float64   `json:"percent,omitempty"`
	SpeedBps   float64   `json:"speed_bps,omitempty"`
	ETASec     float64   `json:"eta_sec,omitempty"`
	Resumed    bool      `json:"resumed,omitempty"`
	Skipped    bool      `json:"skipped,omitempty"` // file already there
	SHA256     string    `json:"sha256,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

type dl struct {
	mu       sync.Mutex
	st       Status
	bytes    atomic.Int64
	cancel   context.CancelFunc
	finished chan struct{}
	// speed sampling
	lastT time.Time
	lastB int64
	speed float64
}

// Manager runs downloads.
type Manager struct {
	Client *http.Client
	mu     sync.Mutex
	all    map[string]*dl
}

// NewManager returns a Manager with a client suited to multi-GB transfers:
// no overall timeout, but a bounded wait for response headers.
func NewManager() *Manager {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	// Transparent gzip would make the .part hold decoded bytes while a
	// resumed Range request returns raw ones.
	tr.DisableCompression = true
	return &Manager{Client: &http.Client{Transport: tr}, all: map[string]*dl{}}
}

// ResolveURL expands hf:// URLs.
func ResolveURL(raw string) (string, error) {
	if rest, ok := strings.CutPrefix(raw, "hf://"); ok {
		rev := "main"
		if i := strings.LastIndex(rest, "@"); i > 0 {
			rest, rev = rest[:i], rest[i+1:]
		}
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 3 || parts[2] == "" {
			return "", fmt.Errorf("hf url must be hf://<owner>/<repo>/<file>[@<revision>]")
		}
		return "https://huggingface.co/" + parts[0] + "/" + parts[1] + "/resolve/" + url.PathEscape(rev) + "/" + parts[2], nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be http(s):// or hf://")
	}
	return raw, nil
}

// Start begins a download in the background.
func (m *Manager) Start(req Request) (Status, error) {
	u, err := ResolveURL(req.URL)
	if err != nil {
		return Status{}, err
	}
	if req.Dest == "" {
		return Status{}, errors.New("dest is required")
	}
	trailing := strings.HasSuffix(req.Dest, "/") || strings.HasSuffix(req.Dest, `\`)
	if req.Dest, err = filepath.Abs(req.Dest); err != nil {
		return Status{}, err
	}
	if trailing {
		req.Dest += string(filepath.Separator)
	}
	if req.SHA256 != "" {
		req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
		if b, err := hex.DecodeString(req.SHA256); err != nil || len(b) != sha256.Size {
			return Status{}, errors.New("sha256 must be 64 hex characters")
		}
	}
	// HuggingFace gated/private repos: use the agent's HF_TOKEN unless the
	// caller sent its own Authorization.
	if strings.HasPrefix(u, "https://huggingface.co/") && os.Getenv("HF_TOKEN") != "" {
		if _, ok := req.Headers["Authorization"]; !ok {
			h := map[string]string{"Authorization": "Bearer " + os.Getenv("HF_TOKEN")}
			for k, v := range req.Headers {
				h[k] = v
			}
			req.Headers = h
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := &dl{cancel: cancel, finished: make(chan struct{}),
		st: Status{ID: "d-" + uuid.NewString()[:8], URL: u, Dest: req.Dest, State: Running, StartedAt: time.Now()}}
	m.mu.Lock()
	for _, o := range m.all {
		if s := o.status(); (s.State == Running || s.State == Verifying) && s.Dest == req.Dest && !trailing {
			m.mu.Unlock()
			cancel()
			return Status{}, fmt.Errorf("download %s is already writing %s", s.ID, req.Dest)
		}
	}
	m.all[d.st.ID] = d
	m.mu.Unlock()
	go m.run(ctx, d, u, req)
	return d.status(), nil
}

func (d *dl) status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.st
	st.Bytes = d.bytes.Load()
	if st.State == Running {
		now := time.Now()
		if dt := now.Sub(d.lastT).Seconds(); d.lastT.IsZero() || dt >= 1 {
			if !d.lastT.IsZero() {
				d.speed = float64(st.Bytes-d.lastB) / dt
			}
			d.lastT, d.lastB = now, st.Bytes
		}
		st.SpeedBps = d.speed
		if st.Total > 0 && d.speed > 0 {
			st.ETASec = float64(st.Total-st.Bytes) / d.speed
		}
	}
	if st.Total > 0 {
		st.Percent = float64(int(float64(st.Bytes)/float64(st.Total)*1000)) / 10
	}
	return st
}

func (d *dl) set(f func(*Status)) {
	d.mu.Lock()
	f(&d.st)
	d.mu.Unlock()
}

func (m *Manager) run(ctx context.Context, d *dl, u string, req Request) {
	defer close(d.finished)
	err := m.fetch(ctx, d, u, req)
	d.set(func(s *Status) {
		s.FinishedAt = time.Now()
		switch {
		case err == nil:
			s.State = Done
		case ctx.Err() != nil:
			s.State, s.Error = Canceled, "canceled (the .part file is kept; start again to resume)"
		default:
			s.State, s.Error = Failed, err.Error()
		}
	})
}

// fetch downloads with up to 5 attempts, resuming from the .part file.
func (m *Manager) fetch(ctx context.Context, d *dl, u string, req Request) error {
	dest := req.Dest
	var err error
	delay := 2 * time.Second
	attempts := req.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	for attempt := 1; ; attempt++ {
		dest, err = m.attempt(ctx, d, u, req, dest)
		if err == nil || ctx.Err() != nil || attempt >= attempts || isPermanent(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

type permanentError struct{ error }

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// attempt performs one HTTP transfer and returns the resolved destination.
func (m *Manager) attempt(ctx context.Context, d *dl, u string, req Request, dest string) (string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return dest, permanentError{err}
	}
	for k, v := range req.Headers {
		r.Header.Set(k, v)
	}

	// The final name may depend on the response (directory dest), so the
	// first attempt resolves it; we only know the .part once dest is a file.
	isDir := strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, `\`)
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		isDir = true
	}
	var part string
	var have int64
	if !isDir {
		if done, err := m.alreadyThere(ctx, d, dest, req); done || err != nil {
			return dest, err
		}
		part = dest + ".part"
		if fi, err := os.Stat(part); err == nil {
			have = fi.Size()
			r.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		}
	}

	resp, err := m.Client.Do(r)
	if err != nil {
		return dest, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0:
		// The .part already holds everything.
		d.bytes.Store(have)
		return dest, m.finish(ctx, d, part, dest, req)
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		d.set(func(s *Status) { s.Resumed = true })
	case resp.StatusCode == http.StatusOK:
		have = 0 // server ignored Range: start over
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return dest, permanentError{fmt.Errorf("HTTP %s (gated/private file? set HF_TOKEN on the agent or pass an Authorization header)", resp.Status)}
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests:
		return dest, permanentError{fmt.Errorf("HTTP %s", resp.Status)}
	default:
		return dest, fmt.Errorf("HTTP %s", resp.Status)
	}

	if isDir {
		dest = filepath.Join(dest, fileName(resp))
		d.set(func(s *Status) { s.Dest = dest })
		if done, err := m.alreadyThere(ctx, d, dest, req); done || err != nil {
			return dest, err
		}
		part = dest + ".part"
		if fi, err := os.Stat(part); err == nil && fi.Size() > 0 {
			// A previous run left a .part: retry, this time with Range.
			return dest, errors.New("resuming existing .part")
		}
	}

	total := int64(0)
	if resp.ContentLength > 0 {
		total = have + resp.ContentLength
	}
	d.set(func(s *Status) { s.Total = total })
	d.bytes.Store(have)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return dest, permanentError{err}
	}
	if total > 0 {
		if u, err := disk.Usage(filepath.Dir(dest)); err == nil && u.Free < uint64(total-have) {
			return dest, permanentError{fmt.Errorf("not enough disk space: need %s, free %s", human(total-have), human(int64(u.Free)))}
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if have > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return dest, permanentError{err}
	}
	_, err = io.Copy(f, &counter{r: resp.Body, n: &d.bytes})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return dest, err
	}
	if total > 0 && d.bytes.Load() != total {
		return dest, fmt.Errorf("connection closed early (%d of %d bytes)", d.bytes.Load(), total)
	}
	return dest, m.finish(ctx, d, part, dest, req)
}

// alreadyThere skips the download when dest exists, unless overwrite. With a
// sha256 the existing file must match to count.
func (m *Manager) alreadyThere(ctx context.Context, d *dl, dest string, req Request) (bool, error) {
	fi, err := os.Stat(dest)
	if err != nil || fi.IsDir() || req.Overwrite {
		return false, nil
	}
	if req.SHA256 != "" {
		d.set(func(s *Status) { s.State = Verifying })
		sum, err := hashFile(ctx, dest)
		d.set(func(s *Status) { s.State = Running })
		if err != nil || sum != req.SHA256 {
			return false, nil
		}
		d.set(func(s *Status) { s.SHA256 = sum })
	}
	d.bytes.Store(fi.Size())
	d.set(func(s *Status) { s.Skipped, s.Total = true, fi.Size() })
	return true, nil
}

// finish verifies the .part and moves it into place.
func (m *Manager) finish(ctx context.Context, d *dl, part, dest string, req Request) error {
	if req.SHA256 != "" {
		d.set(func(s *Status) { s.State = Verifying })
		sum, err := hashFile(ctx, part)
		if err != nil {
			return permanentError{err}
		}
		if sum != req.SHA256 {
			// Wrong file or corrupt transfer; either way retrying blindly
			// rarely helps. Drop the .part so a new download starts clean.
			_ = os.Remove(part)
			return permanentError{fmt.Errorf("sha256 mismatch: got %s, want %s", sum, req.SHA256)}
		}
		d.set(func(s *Status) { s.SHA256 = sum })
	}
	if req.Overwrite {
		_ = os.Remove(dest)
	}
	if err := os.Rename(part, dest); err != nil {
		return permanentError{err}
	}
	return nil
}

// hashFile stops when ctx is done: hashing 20 GB takes a minute or two and
// Cancel waits for it.
func hashFile(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileName picks the name for a directory dest: Content-Disposition, then
// the last segment of the final (post-redirect) URL.
func fileName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if n := filepath.Base(params["filename"]); n != "" && n != "." && n != "/" {
			return n
		}
	}
	if n := path.Base(resp.Request.URL.Path); n != "" && n != "/" && n != "." {
		return n
	}
	return "download"
}

type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (m *Manager) get(id string) (*dl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.all[id]
	if !ok {
		return nil, fmt.Errorf("no download %q", id)
	}
	return d, nil
}

// Get returns a download's status, waiting up to wait for it to finish.
func (m *Manager) Get(ctx context.Context, id string, wait time.Duration) (Status, error) {
	d, err := m.get(id)
	if err != nil {
		return Status{}, err
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-d.finished:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return d.status(), nil
}

// List returns all downloads, newest first.
func (m *Manager) List() []Status {
	m.mu.Lock()
	ds := make([]*dl, 0, len(m.all))
	for _, d := range m.all {
		ds = append(ds, d)
	}
	m.mu.Unlock()
	out := make([]Status, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.status())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// Cancel stops a download, keeping its .part for a later resume.
func (m *Manager) Cancel(id string) (Status, error) {
	d, err := m.get(id)
	if err != nil {
		return Status{}, err
	}
	d.cancel()
	<-d.finished
	return d.status(), nil
}

// StopAll cancels every download (agent shutdown); .part files are kept.
func (m *Manager) StopAll() {
	m.mu.Lock()
	ds := make([]*dl, 0, len(m.all))
	for _, d := range m.all {
		ds = append(ds, d)
	}
	m.mu.Unlock()
	for _, d := range ds {
		d.cancel()
		<-d.finished
	}
}

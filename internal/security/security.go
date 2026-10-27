// Package security provides the agent's authorization guard and audit log.
//
// The guard is a defense-in-depth layer on top of the bearer token and the
// WireGuard transport: it can restrict which capabilities are callable
// (allow/deny lists) and expose a runtime "lock" that denies everything except
// admin control — a kill switch the operator (or Hermes itself) can flip.
package security

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// Guard decides whether a method may run right now.
type Guard struct {
	mu     sync.RWMutex
	allow  map[string]bool // if non-empty, only these methods are permitted
	deny   map[string]bool // always rejected
	locked bool            // when true, only admin.* methods run
}

// NewGuard builds a guard from optional allow/deny method lists.
func NewGuard(allow, deny []string) *Guard {
	g := &Guard{allow: toSet(allow), deny: toSet(deny)}
	return g
}

func toSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			m[x] = true
		}
	}
	return m
}

// Check returns nil if method may run, or a denied protocol.Error otherwise.
func (g *Guard) Check(method string) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	isAdmin := strings.HasPrefix(method, "admin.")
	if g.locked && !isAdmin {
		return &protocol.Error{Code: protocol.CodeDenied, Message: "agent is locked (kill switch active); only admin.* methods allowed"}
	}
	if match(g.deny, method) {
		return &protocol.Error{Code: protocol.CodeDenied, Message: "method is denied by configuration"}
	}
	if len(g.allow) > 0 && !match(g.allow, method) && !isAdmin {
		return &protocol.Error{Code: protocol.CodeDenied, Message: "method is not in the allow list"}
	}
	return nil
}

// match reports whether method is in set, either literally or through a
// "prefix.*" entry (e.g. "task.*" covers every AI-defined task).
func match(set map[string]bool, method string) bool {
	if set[method] {
		return true
	}
	for i := len(method) - 1; i > 0; i-- {
		if method[i] == '.' && set[method[:i]+".*"] {
			return true
		}
	}
	return false
}

// Lock engages the kill switch. SetLock(false) releases it.
func (g *Guard) SetLock(v bool) {
	g.mu.Lock()
	g.locked = v
	g.mu.Unlock()
}

// Locked reports the current lock state.
func (g *Guard) Locked() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.locked
}

// Audit appends one JSON line per invocation to a file. Safe for concurrent use.
type Audit struct {
	mu     sync.Mutex
	path   string
	f      *os.File // nil after a failed reopen; Log tries again
	size   int64
	max    int64
	notify func(Entry)
}

// maxAuditSize is the size past which the audit log is moved to <path>.1
// (replacing the previous one) and a new one is started. The panel polls
// sys.stats and service.status every few seconds and each call is a line, so
// without this the log grew to tens of MB on every machine.
const maxAuditSize = 16 << 20

// Notify calls fn after every logged entry (set once, at startup).
func (a *Audit) Notify(fn func(Entry)) {
	if a != nil {
		a.notify = fn
	}
}

// NewAudit opens (or creates) the audit log at path. A nil Audit is a no-op.
func NewAudit(path string) (*Audit, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	a := &Audit{max: maxAuditSize}
	if err := a.open(path); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Audit) open(path string) error {
	a.path = path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	a.f, a.size = f, 0
	if st, err := f.Stat(); err == nil {
		a.size = st.Size()
	}
	return nil
}

// rotate moves the log to <path>.1 and opens a new one. Called with a.mu held.
// os.Rename replaces <path>.1 on every OS, so the old .1 is only lost once the
// new one is in place. If the rename fails (another process has the file open
// on Windows), it keeps appending to the current file and tries again after
// another max bytes.
func (a *Audit) rotate() {
	a.f.Close()
	a.f = nil
	renamed := os.Rename(a.path, a.path+".1") == nil
	if err := a.open(a.path); err != nil {
		return // a.f stays nil: Log reopens on the next call
	}
	if !renamed {
		a.size = 0
	}
}

// Entry is one audited invocation.
type Entry struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Args   string `json:"args,omitempty"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
	// Work is the declared task the call ran under (see package work).
	Work string `json:"work,omitempty"`
}

// maxAuditArgs bounds the arguments recorded per entry (fs.write payloads can
// be megabytes of base64).
const maxAuditArgs = 2 << 10

// Log records an invocation and its (truncated) arguments. No-op if a is nil.
// The declared task in ctx (protocol.WorkID), if any, is recorded with it.
func (a *Audit) Log(ctx context.Context, method string, args json.RawMessage, err error) {
	if a == nil {
		return
	}
	e := Entry{Time: time.Now().UTC().Format(time.RFC3339), Method: method, OK: err == nil, Work: protocol.WorkID(ctx)}
	if s := string(redact(args)); s != "" && s != "null" && s != "{}" {
		if len(s) > maxAuditArgs {
			s = s[:maxAuditArgs] + "…"
		}
		e.Args = s
	}
	if err != nil {
		e.Err = err.Error()
	}
	data, _ := json.Marshal(e)
	a.mu.Lock()
	if a.f == nil && a.path != "" {
		a.open(a.path)
	}
	if a.f != nil {
		if a.max > 0 && a.size >= a.max {
			a.rotate()
		}
		n, _ := a.f.Write(append(data, '\n'))
		a.size += int64(n)
	}
	a.mu.Unlock()
	if a.notify != nil {
		a.notify(e)
	}
}

// Path is the audit file location ("" if disabled).
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.path
}

// ReadAudit returns the last limit entries of the audit log at path, newest
// first. It reads at most the final 4 MiB, so a huge log stays cheap.
func ReadAudit(path string, limit int) ([]Entry, error) {
	return ReadAuditFunc(path, limit, nil)
}

// ReadAuditFunc is ReadAudit keeping only the entries keep accepts (all if nil).
// Right after a rotation the current file is short, so the rest comes from
// the rotated <path>.1.
func ReadAuditFunc(path string, limit int, keep func(Entry) bool) ([]Entry, error) {
	out, err := readAuditFile(path, limit, keep)
	if err != nil || len(out) >= limit {
		return out, err
	}
	older, err := readAuditFile(path+".1", limit-len(out), keep)
	if err != nil {
		return out, nil
	}
	return append(out, older...), nil
}

func readAuditFile(path string, limit int, keep func(Entry) bool) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const window = 4 << 20
	if st, err := f.Stat(); err == nil && st.Size() > window {
		if _, err := f.Seek(st.Size()-window, io.SeekStart); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []Entry
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		var e Entry
		if json.Unmarshal([]byte(lines[i]), &e) == nil && e.Method != "" && (keep == nil || keep(e)) {
			out = append(out, e)
		}
	}
	return out, nil
}

// Close closes the audit file.
func (a *Audit) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f != nil {
		a.f.Close()
		a.f = nil
	}
	a.path = "" // closed for good: Log must not reopen it
}

// secretKeys are argument fields whose values never reach the audit log or
// the panel's feed: recipe.apply secrets and source tokens, download headers.
var secretKeys = map[string]bool{"secrets": true, "token": true, "password": true, "authorization": true}

// secretMarks are the spellings of secretKeys looked for before parsing.
var secretMarks = [][]byte{[]byte(`"secrets"`), []byte(`"token"`), []byte(`"password"`),
	[]byte(`"authorization"`), []byte(`"Authorization"`), []byte(`"Token"`), []byte(`"Password"`)}

// redact replaces the values of secretKeys, at any depth, with "***". Args
// that mention none of them are returned untouched (and unparsed: fs.write
// payloads can be megabytes).
func redact(args json.RawMessage) json.RawMessage {
	found := false
	for _, k := range secretMarks {
		if bytes.Contains(args, k) {
			found = true
			break
		}
	}
	if !found {
		return args
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return json.RawMessage(`"(unparsable arguments with secrets, not recorded)"`)
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return nil
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if secretKeys[strings.ToLower(k)] {
				t[k] = "***"
			} else {
				t[k] = redactValue(x)
			}
		}
	case []any:
		for i, x := range t {
			t[i] = redactValue(x)
		}
	}
	return v
}

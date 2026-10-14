// Package security provides the agent's authorization guard and audit log.
//
// The guard is a defense-in-depth layer on top of the bearer token and the
// WireGuard transport: it can restrict which capabilities are callable
// (allow/deny lists) and expose a runtime "lock" that denies everything except
// admin control — a kill switch the operator (or Hermes itself) can flip.
package security

import (
	"encoding/json"
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
	mu sync.Mutex
	f  *os.File
}

// NewAudit opens (or creates) the audit log at path. A nil Audit is a no-op.
func NewAudit(path string) (*Audit, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f}, nil
}

// Entry is one audited invocation.
type Entry struct {
	Time   string `json:"time"`
	Method string `json:"method"`
	Args   string `json:"args,omitempty"`
	OK     bool   `json:"ok"`
	Err    string `json:"err,omitempty"`
}

// maxAuditArgs bounds the arguments recorded per entry (fs.write payloads can
// be megabytes of base64).
const maxAuditArgs = 2 << 10

// Log records an invocation and its (truncated) arguments. No-op if a is nil.
func (a *Audit) Log(method string, args json.RawMessage, err error) {
	if a == nil {
		return
	}
	e := Entry{Time: time.Now().UTC().Format(time.RFC3339), Method: method, OK: err == nil}
	if s := string(args); s != "" && s != "null" && s != "{}" {
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
	defer a.mu.Unlock()
	a.f.Write(append(data, '\n'))
}

// Close closes the audit file.
func (a *Audit) Close() {
	if a != nil && a.f != nil {
		a.f.Close()
	}
}

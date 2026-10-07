// Package auth provides the shared-secret check for the agent's control channel.
//
// Over a Tailscale tailnet the transport is already encrypted and the peer set
// is already restricted, so a bearer token is sufficient as a second factor
// that also survives if the agent is ever bound beyond the tailnet by mistake.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Token holds the expected bearer secret.
type Token struct {
	expected string
}

// New returns a Token. An empty secret disables auth (dev only) and callers
// should warn loudly in that case.
func New(secret string) *Token { return &Token{expected: secret} }

// Enabled reports whether a non-empty secret is configured.
func (t *Token) Enabled() bool { return t.expected != "" }

// Check validates an "Authorization: Bearer <token>" header in constant time.
func (t *Token) Check(r *http.Request) bool {
	if !t.Enabled() {
		return true
	}
	h := r.Header.Get("Authorization")
	got := strings.TrimPrefix(h, "Bearer ")
	if got == h { // prefix was absent
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(t.expected)) == 1
}

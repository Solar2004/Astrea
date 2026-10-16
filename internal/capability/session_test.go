package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Solar2004/Astrea/internal/protocol"
)

// TestSessionSchemas checks the advertised input schemas are valid JSON.
func TestSessionSchemas(t *testing.T) {
	for _, c := range []Capability{SessionNoLock{}, SessionUnlock{}} {
		var v any
		if err := json.Unmarshal(c.Describe().InputSchema, &v); err != nil {
			t.Errorf("%s: invalid input schema: %v", c.Name(), err)
		}
	}
	if (SessionNoLock{}).Name() != "session.nolock" {
		t.Error("nolock name")
	}
	if (SessionUnlock{}).Name() != "session.unlock" {
		t.Error("unlock name")
	}
}

// TestSessionUnsupported: when a backend is not wired for the OS (every OS but
// Windows), the capability reports "unavailable" rather than panicking.
func TestSessionUnsupported(t *testing.T) {
	if nolock == nil {
		_, err := SessionNoLock{}.Invoke(context.Background(), json.RawMessage(`{"on":true}`), nil)
		assertUnavailable(t, "session.nolock", err)
	}
	if unlock == nil {
		_, err := SessionUnlock{}.Invoke(context.Background(), json.RawMessage(`{}`), nil)
		assertUnavailable(t, "session.unlock", err)
	}
}

func assertUnavailable(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error with no backend", name)
	}
	pe, ok := err.(*protocol.Error)
	if !ok || pe.Code != protocol.CodeUnavailable {
		t.Fatalf("%s: want CodeUnavailable, got %v", name, err)
	}
}

// TestSessionNoLockBadPayload: malformed JSON is a bad request, not a panic
// (only exercised where a backend is present).
func TestSessionNoLockBadPayload(t *testing.T) {
	if nolock == nil {
		t.Skip("no nolock backend on this OS")
	}
	if _, err := (SessionNoLock{}).Invoke(context.Background(), json.RawMessage(`{bad`), nil); err == nil {
		t.Error("expected error on bad payload")
	}
}

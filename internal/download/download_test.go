package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var payload = bytes.Repeat([]byte("astrea-model-weights-"), 50_000) // ~1 MB

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// server serves payload with Range support and counts requests that asked
// for a range.
func server(t *testing.T, ranged *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
		}
		if r.URL.Path == "/named" {
			w.Header().Set("Content-Disposition", `attachment; filename="qwen.gguf"`)
		}
		if r.URL.Path == "/secret" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		http.ServeContent(w, r, "x.bin", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wait(t *testing.T, m *Manager, id string) Status {
	t.Helper()
	st, err := m.Get(context.Background(), id, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDownloadVerifyAndSkip(t *testing.T) {
	var ranged atomic.Int32
	srv := server(t, &ranged)
	m := NewManager()
	dest := filepath.Join(t.TempDir(), "sub", "model.bin")

	st, err := m.Start(Request{URL: srv.URL + "/f", Dest: dest, SHA256: strings.ToUpper(sum(payload))})
	if err != nil {
		t.Fatal(err)
	}
	if st = wait(t, m, st.ID); st.State != Done || st.Bytes != int64(len(payload)) || st.SHA256 != sum(payload) {
		t.Fatalf("got %+v", st)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, payload) {
		t.Fatal("content differs")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}

	// Second time: already there and verified, nothing transferred.
	st, _ = m.Start(Request{URL: srv.URL + "/f", Dest: dest, SHA256: sum(payload)})
	if st = wait(t, m, st.ID); st.State != Done || !st.Skipped {
		t.Fatalf("not skipped: %+v", st)
	}
}

func TestDownloadResume(t *testing.T) {
	var ranged atomic.Int32
	srv := server(t, &ranged)
	m := NewManager()
	dest := filepath.Join(t.TempDir(), "model.bin")
	half := len(payload) / 2
	os.WriteFile(dest+".part", payload[:half], 0o644)

	st, _ := m.Start(Request{URL: srv.URL + "/f", Dest: dest, SHA256: sum(payload)})
	if st = wait(t, m, st.ID); st.State != Done || !st.Resumed || ranged.Load() != 1 {
		t.Fatalf("got %+v ranged=%d", st, ranged.Load())
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, payload) {
		t.Fatal("resumed content differs")
	}
}

func TestDownloadBadHash(t *testing.T) {
	var ranged atomic.Int32
	srv := server(t, &ranged)
	m := NewManager()
	dest := filepath.Join(t.TempDir(), "model.bin")
	st, _ := m.Start(Request{URL: srv.URL + "/f", Dest: dest, SHA256: strings.Repeat("0", 64)})
	st, _ = m.Get(context.Background(), st.ID, 60*time.Second)
	if st.State != Failed || !strings.Contains(st.Error, "sha256 mismatch") {
		t.Fatalf("got %+v", st)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("corrupt file moved into place")
	}
}

func TestDownloadDirDestAndErrors(t *testing.T) {
	var ranged atomic.Int32
	srv := server(t, &ranged)
	m := NewManager()
	dir := t.TempDir()
	st, _ := m.Start(Request{URL: srv.URL + "/named", Dest: dir})
	if st = wait(t, m, st.ID); st.State != Done || st.Dest != filepath.Join(dir, "qwen.gguf") {
		t.Fatalf("got %+v", st)
	}

	// 401 is permanent: fails at once instead of retrying.
	start := time.Now()
	st, _ = m.Start(Request{URL: srv.URL + "/secret", Dest: filepath.Join(dir, "s")})
	if st = wait(t, m, st.ID); st.State != Failed || !strings.Contains(st.Error, "HF_TOKEN") || time.Since(start) > time.Second {
		t.Fatalf("got %+v", st)
	}

	if _, err := m.Start(Request{URL: "ftp://x/y", Dest: dir}); err == nil {
		t.Fatal("ftp accepted")
	}
	if _, err := m.Start(Request{URL: srv.URL, Dest: dir, SHA256: "abc"}); err == nil {
		t.Fatal("bad sha accepted")
	}
}

func TestResolveHF(t *testing.T) {
	for in, want := range map[string]string{
		"hf://Qwen/Qwen3-GGUF/q4.gguf":          "https://huggingface.co/Qwen/Qwen3-GGUF/resolve/main/q4.gguf",
		"hf://Qwen/Qwen3-GGUF/sub/q4.gguf@v1.0": "https://huggingface.co/Qwen/Qwen3-GGUF/resolve/v1.0/sub/q4.gguf",
	} {
		if got, err := ResolveURL(in); err != nil || got != want {
			t.Errorf("ResolveURL(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ResolveURL("hf://only/repo"); err == nil {
		t.Error("hf url without file accepted")
	}
}

package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v0.2.1", "v0.3.0", true},
		{"v0.2.1", "v0.2.1", false},
		{"v0.10.0", "v0.9.9", false},
		{"0.2.1", "v0.2.2", true},
		{"dev", "v9.9.9", false},
		{"v0.2.1", "garbage", false},
		{"v1.0.0-rc1", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.cur, c.latest, got, c.want)
		}
	}
}

// fakeRelease serves /latest, SHA256SUMS.txt and one asset.
func fakeRelease(t *testing.T, tag string, body []byte, sum string) *httptest.Server {
	asset := AssetName("astrea-agent", runtime.GOOS, runtime.GOARCH)
	if sum == "" {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/download/"+tag+"/SHA256SUMS.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\nabc  other\n", sum, asset)
	})
	mux.HandleFunc("/download/"+tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestAndApply(t *testing.T) {
	srv := fakeRelease(t, "v0.9.0", []byte("NEW"), "")
	ctx := context.Background()
	tag, err := Latest(ctx, srv.URL)
	if err != nil || tag != "v0.9.0" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
	exe := filepath.Join(t.TempDir(), "astrea-agent")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	if err := Apply(ctx, srv.URL, "astrea-agent", tag, exe, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Fatalf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "OLD" {
		t.Fatalf("old = %q", b)
	}
	CleanupOld(exe)
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatal(".old not cleaned")
	}
}

func TestApplyBadChecksumKeepsBinary(t *testing.T) {
	srv := fakeRelease(t, "v0.9.0", []byte("EVIL"), "00ff")
	exe := filepath.Join(t.TempDir(), "astrea-agent")
	os.WriteFile(exe, []byte("OLD"), 0o755)
	if err := Apply(context.Background(), srv.URL, "astrea-agent", "v0.9.0", exe, nil); err == nil {
		t.Fatal("expected checksum error")
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("exe changed to %q", b)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal(".new left behind")
	}
}

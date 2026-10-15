// Package update keeps Astrea binaries current from GitHub Releases.
//
// It finds the latest tag through the /releases/latest redirect (no GitHub
// API, so no rate limit), downloads the asset for this component and
// platform, checks it against the release's SHA256SUMS.txt and swaps it in
// place of the running executable. The old file is kept as <exe>.old (a
// running Windows executable can be renamed but not deleted) and removed on
// the next start by CleanupOld.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBase is where releases are published. ASTREA_UPDATE_URL overrides
// it (a mirror, or a local server while testing).
const DefaultBase = "https://github.com/Solar2004/Astrea/releases"

// Base returns the releases base URL in effect.
func Base() string {
	if b := os.Getenv("ASTREA_UPDATE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return DefaultBase
}

// Client is the HTTP client used for checks and downloads. It does not follow
// redirects on its own for the latest-tag lookup (see Latest).
var Client = &http.Client{Timeout: 10 * time.Minute}

// Latest returns the tag of the newest release, e.g. "v0.3.0".
func Latest(ctx context.Context, base string) (string, error) {
	c := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("check latest release: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("check latest release: %s returned %s without a redirect", base+"/latest", resp.Status)
	}
	tag := path.Base(loc)
	if _, ok := parse(tag); !ok {
		return "", fmt.Errorf("check latest release: unexpected redirect %q", loc)
	}
	return tag, nil
}

// parse reads "v1.2.3" (optionally with a "-suffix") into its numbers.
func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether latest is a strictly higher version than current.
// Unversioned builds ("dev") are never considered outdated: they come from a
// source tree and should not be silently replaced by a release.
func Newer(current, latest string) bool {
	c, ok1 := parse(current)
	l, ok2 := parse(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// AssetName is the release file holding component ("astrea-agent",
// "astrea-hub", "astrea-panel") for goos/goarch, matching release.yml.
func AssetName(component, goos, goarch string) string {
	if goos == "windows" {
		return component + ".exe"
	}
	return fmt.Sprintf("%s-%s-%s", component, goos, goarch)
}

// Result describes what Apply did.
type Result struct {
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
	Path      string `json:"path"`
	Updated   bool   `json:"updated"`
}

// Progress receives human-readable progress lines; may be nil.
type Progress func(string)

// Apply downloads component at tag for this platform and replaces exe with it.
// exe is usually the running executable (Self).
func Apply(ctx context.Context, base, component, tag, exe string, progress Progress) error {
	if progress == nil {
		progress = func(string) {}
	}
	asset := AssetName(component, runtime.GOOS, runtime.GOARCH)
	sums, err := fetchSums(ctx, base, tag)
	if err != nil {
		return err
	}
	want, ok := sums[asset]
	if !ok {
		return fmt.Errorf("release %s has no %s (SHA256SUMS.txt does not list it)", tag, asset)
	}

	tmp := exe + ".new"
	progress(fmt.Sprintf("descargando %s %s", asset, tag))
	if err := download(ctx, base+"/download/"+tag+"/"+asset, tmp, want); err != nil {
		os.Remove(tmp)
		return err
	}
	progress("checksum OK, sustituyendo " + exe)
	if err := Replace(exe, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Replace puts newFile in place of exe, keeping the previous one as exe.old
// and restoring it if the swap fails halfway.
func Replace(exe, newFile string) error {
	old := exe + ".old"
	_ = os.Remove(old) // a leftover from an earlier update, if no longer in use
	if err := os.Rename(exe, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Windows keeps an old .old locked while a process still runs it;
			// pick a fresh name instead of failing.
			old = fmt.Sprintf("%s.%d.old", exe, time.Now().Unix())
			if err2 := os.Rename(exe, old); err2 != nil {
				return fmt.Errorf("move current binary aside: %w", err)
			}
		} else {
			old = ""
		}
	}
	if err := os.Rename(newFile, exe); err != nil {
		if old != "" {
			_ = os.Rename(old, exe)
		}
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// CleanupOld removes binaries left behind by earlier updates. Files still in
// use (Windows) are skipped; a later start gets them.
func CleanupOld(exe string) {
	matches, _ := filepath.Glob(exe + "*.old")
	for _, m := range matches {
		_ = os.Remove(m)
	}
	_ = os.Remove(exe + ".new")
}

// Self returns the path of the running executable with symlinks resolved.
// The first answer is remembered: once Apply has renamed the running file,
// Linux reports it under its new name (exe.old), and a restart must launch
// the new binary at the original path.
func Self() (string, error) {
	selfOnce.Do(func() {
		selfPath, selfErr = os.Executable()
		if selfErr == nil {
			if r, err := filepath.EvalSymlinks(selfPath); err == nil {
				selfPath = r
			}
		}
	})
	return selfPath, selfErr
}

var (
	selfOnce sync.Once
	selfPath string
	selfErr  error
)

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// fetchSums reads SHA256SUMS.txt of a release into name → hex digest.
func fetchSums(ctx context.Context, base, tag string) (map[string]string, error) {
	resp, err := get(ctx, base+"/download/"+tag+"/SHA256SUMS.txt")
	if err != nil {
		return nil, fmt.Errorf("checksums: %w", err)
	}
	defer resp.Body.Close()
	sums := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return sums, sc.Err()
}

// download writes url to dst (mode 0755) and checks its SHA-256.
func download(ctx context.Context, url, dst, wantHex string) error {
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHex {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", path.Base(url), got, wantHex)
	}
	return nil
}

package recipe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	st, _ := OpenStore("")
	r := &Recipe{Schema: Schema, Name: "demo", Version: "1.0.0", Root: "${data}/demo",
		Artifacts: []Artifact{
			{ID: "m", Path: "models/m.bin", Size: 10},
			{ID: "half", Path: "models/half.bin", Size: 10},
		},
		Files: []File{{Path: "conf.yaml", Content: "secret"}},
	}
	st.Update("demo", func(it *Installed) { it.Recipe, it.State = raw(r), StateInstalled })
	root := filepath.Join(dir, "demo")
	os.MkdirAll(filepath.Join(root, "models"), 0o755)
	os.WriteFile(filepath.Join(root, "models", "m.bin"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(root, "models", "half.bin"), []byte("01234"), 0o644)
	os.WriteFile(filepath.Join(root, "conf.yaml"), []byte("secret"), 0o644)
	srv := httptest.NewServer(&Files{Store: st, Data: dir})
	defer srv.Close()

	get := func(path, rng string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/files/demo/models/m.bin", ""); code != 200 || body != "0123456789" {
		t.Errorf("full: %d %q", code, body)
	}
	if code, body := get("/files/demo/models/m.bin", "bytes=4-"); code != 206 || body != "456789" {
		t.Errorf("range: %d %q", code, body)
	}
	for _, p := range []string{
		"/files/demo/models/half.bin",      // incomplete
		"/files/demo/conf.yaml",            // not an artifact
		"/files/demo/../demo/models/m.bin", // not listed as is
		"/files/other/models/m.bin",        // not installed
		"/files/demo/",
	} {
		if code, _ := get(p, ""); code != 404 {
			t.Errorf("%s: %d, want 404", p, code)
		}
	}
}

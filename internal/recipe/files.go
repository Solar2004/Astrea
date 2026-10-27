package recipe

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// RootFor expands only ${data} in Root, which is all /files/ needs (the rest
// of the recipe may use secrets this machine no longer has).
func (r *Recipe) RootFor(data string) (string, error) {
	x := &Recipe{Root: r.Root}
	out, err := x.Expand(Vars{Data: data})
	if err != nil {
		return "", err
	}
	// Relative would mean the agent's working directory: never serve or
	// purge there.
	if !filepath.IsAbs(out.Root) {
		return "", fmt.Errorf("root %q is not an absolute path on this machine", out.Root)
	}
	return out.Root, nil
}

// Files serves the artifacts of the recipes installed on this machine at
// /files/<recipe>/<artifact path>, with Range support, so other machines can
// copy them instead of going to the internet. Only files listed as artifacts
// and complete (exact size) are served: never configs, never anything else
// under root. Authentication is the caller's job.
type Files struct {
	Store *Store
	Data  string
}

func (h *Files) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/files/")
	name, rel, ok := strings.Cut(rest, "/")
	if !ok || name == "" || rel == "" {
		http.Error(w, "want /files/<recipe>/<path>", http.StatusNotFound)
		return
	}
	it, ok := h.Store.Get(name)
	if !ok {
		http.Error(w, "recipe not installed here", http.StatusNotFound)
		return
	}
	rc, err := Parse(it.Recipe)
	if err != nil {
		http.Error(w, "stored recipe unreadable", http.StatusInternalServerError)
		return
	}
	var art *Artifact
	for i := range rc.Artifacts {
		if rc.Artifacts[i].Path == rel {
			art = &rc.Artifacts[i]
			break
		}
	}
	if art == nil {
		http.Error(w, "not an artifact of "+name, http.StatusNotFound)
		return
	}
	root, err := rc.RootFor(h.Data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(art.Path)))
	if err != nil {
		http.Error(w, "not on this machine", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || (art.Size > 0 && fi.Size() != art.Size) {
		// Incomplete or replaced: serving it would spread a bad copy.
		http.Error(w, "not complete on this machine", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if art.SHA256 != "" {
		w.Header().Set("X-Astrea-SHA256", art.SHA256)
	}
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

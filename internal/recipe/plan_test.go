package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// planRecipe has one artifact per interesting case. Sizes are the length of
// the contents used in the tests.
func planRecipe() *Recipe {
	return &Recipe{
		Schema: Schema, Name: "demo", Version: "1.0.0", Root: "/srv/demo",
		Profiles: map[string]Profile{
			"min":  {Groups: []string{"a"}, Default: true},
			"full": {Groups: []string{"a", "b"}},
		},
		Artifacts: []Artifact{
			{ID: "ok", Group: "a", Path: "m/ok.bin", Size: 5, SHA256: sum("hello"), URLs: []string{"https://x/ok"}},
			{ID: "gone", Group: "a", Path: "m/gone.bin", Size: 10, URLs: []string{"https://x/gone"}},
			{ID: "half", Group: "a", Path: "m/half.bin", Size: 10, URLs: []string{"https://x/half"}},
			{ID: "short", Group: "a", Path: "m/short.bin", Size: 10, URLs: []string{"https://x/short"}},
			{ID: "bad", Group: "a", Path: "m/bad.bin", Size: 5, SHA256: sum("hello"), URLs: []string{"https://x/bad"}},
			{ID: "zip", Group: "a", Path: "dist/app.zip", Size: 7, URLs: []string{"https://x/zip"}, Unpack: &Unpack{To: "app", Creates: "app/run.exe"}},
			{ID: "local", Group: "a", Path: "m/local.bin", Size: 3},
			{ID: "big", Group: "b", Path: "m/big.bin", Size: 4, URLs: []string{"https://x/big"}},
		},
		Files: []File{
			{Path: "conf/same.yaml", Content: "a: 1\n"},
			{Path: "conf/diff.yaml", Content: "a: 2\n"},
			{Path: "conf/new.yaml", Content: "a: 3\n"},
		},
		Setup:    []Step{{Name: "dirs", Run: "mkdir out", Creates: "out"}},
		Services: []Service{{Name: "app", Command: "app/run.exe"}},
	}
}

func planFS() fstest.MapFS {
	return fstest.MapFS{
		"m/ok.bin":        {Data: []byte("hello")},
		"m/half.bin.part": {Data: []byte("1234")},
		"m/short.bin":     {Data: []byte("123")},
		"m/bad.bin":       {Data: []byte("HELLO")},
		"m/big.bin":       {Data: []byte("abcd")},
		"conf/same.yaml":  {Data: []byte("\xef\xbb\xbfa: 1\r\n")}, // BOM + CRLF: same text
		"conf/diff.yaml":  {Data: []byte("a: 1\n")},
	}
}

func byID(p *Plan) map[string]ArtifactPlan {
	m := map[string]ArtifactPlan{}
	for _, a := range p.Artifacts {
		m[a.ID] = a
	}
	return m
}

func TestPlanStates(t *testing.T) {
	r := planRecipe()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := MakePlan(r, planFS(), PlanOptions{Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile != "min" {
		t.Errorf("profile %q", p.Profile)
	}
	want := map[string]struct {
		state         string
		have          int64
		fetch, unpack bool
	}{
		"ok":    {Present, 5, false, false},
		"gone":  {Missing, 0, true, false},
		"half":  {Partial, 4, true, false},
		"short": {WrongSize, 0, true, false},
		"bad":   {WrongHash, 0, true, false},
		"zip":   {Missing, 0, true, true},
		"local": {Missing, 0, true, false},
	}
	got := byID(p)
	if len(got) != len(want) {
		t.Errorf("planned %d artifacts, want %d (big is not in min)", len(got), len(want))
	}
	for id, w := range want {
		a := got[id]
		if a.State != w.state || a.Have != w.have || a.Fetch != w.fetch || a.Unpack != w.unpack {
			t.Errorf("%s: got %+v, want %+v", id, a, w)
		}
	}
	if !got["ok"].Verified {
		t.Error("ok should be verified")
	}
	// 10 gone + 6 left of half + 10 short + 5 bad + 7 zip + 3 local.
	if p.FetchBytes != 41 || p.TotalBytes != 50 {
		t.Errorf("fetch %d total %d, want 41 and 50", p.FetchBytes, p.TotalBytes)
	}
	if len(p.Extra) != 1 || p.Extra[0].ID != "big" {
		t.Errorf("extra %+v, want big", p.Extra)
	}
	files := map[string]string{}
	for _, f := range p.Files {
		files[f.Path] = f.State
	}
	if files["conf/same.yaml"] != FileSame || files["conf/diff.yaml"] != FileDiffers || files["conf/new.yaml"] != FileMissing {
		t.Errorf("files %v", files)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "local") {
		t.Errorf("warnings %v, want one about local having no mirror", p.Warnings)
	}
	if p.Done() {
		t.Error("plan with downloads reported done")
	}
}

func TestPlanWithoutVerifyTrustsSize(t *testing.T) {
	p, err := MakePlan(planRecipe(), planFS(), PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := byID(p)["bad"]; a.State != Present || a.Verified {
		t.Errorf("bad: %+v, want present and not verified", a)
	}
}

// Adopting a machine that already has everything must not download anything,
// even if the archive was deleted after unpacking.
func TestPlanAdopt(t *testing.T) {
	r := planRecipe()
	fsys := fstest.MapFS{"app/run.exe": {Data: []byte("x")}}
	for _, a := range r.Artifacts {
		if a.Unpack == nil {
			fsys[a.Path] = &fstest.MapFile{Data: make([]byte, a.Size)}
		}
	}
	for _, f := range r.Files {
		fsys[f.Path] = &fstest.MapFile{Data: []byte(f.Content)}
	}
	p, err := MakePlan(r, fsys, PlanOptions{Profile: "full"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Done() || p.FetchBytes != 0 {
		t.Fatalf("not done: %+v", p)
	}
	if a := byID(p)["zip"]; a.State != Unpacked {
		t.Errorf("zip: %+v, want unpacked", a)
	}
}

func TestPlanArchivePresentNeedsUnpack(t *testing.T) {
	r := planRecipe()
	fsys := fstest.MapFS{"dist/app.zip": {Data: []byte("1234567")}}
	p, err := MakePlan(r, fsys, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := byID(p)["zip"]; a.State != Present || a.Fetch || !a.Unpack {
		t.Errorf("zip: %+v, want present, no fetch, unpack", a)
	}
}

func TestPlanConflict(t *testing.T) {
	fsys := fstest.MapFS{"m/ok.bin/inner": {Data: []byte("x")}}
	p, err := MakePlan(planRecipe(), fsys, PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := byID(p)["ok"]; a.State != Conflict || a.Fetch {
		t.Errorf("ok: %+v, want conflict without fetch", a)
	}
	if p.Done() {
		t.Error("conflict reported done")
	}
}

func TestPlanNeedsExpand(t *testing.T) {
	r := planRecipe()
	r.Files[0].Content = "token: ${secret:t}"
	if _, err := MakePlan(r, fstest.MapFS{}, PlanOptions{}); err == nil || !strings.Contains(err.Error(), "Expand") {
		t.Fatalf("got %v", err)
	}
}

func TestPlanUsesHasher(t *testing.T) {
	calls := 0
	h := func(fsys fs.FS, name string) (string, error) { calls++; return HashFile(fsys, name) }
	if _, err := MakePlan(planRecipe(), planFS(), PlanOptions{Verify: true, Hash: h}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 { // ok and bad: the only ones with a sha256 and the right size
		t.Errorf("hashed %d files, want 2", calls)
	}
}

// The real ComfyUI recipe against an empty disk downloads the whole profile.
func TestPlanComfyEmpty(t *testing.T) {
	r, err := Load("../../recipes/comfyui-minimax/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := MakePlan(r, fstest.MapFS{}, PlanOptions{Profile: "basico"})
	if err != nil {
		t.Fatal(err)
	}
	if p.FetchBytes != p.TotalBytes || p.TotalBytes == 0 {
		t.Errorf("fetch %d of %d", p.FetchBytes, p.TotalBytes)
	}
	var kitWarned bool
	for _, w := range p.Warnings {
		kitWarned = kitWarned || strings.HasPrefix(w, "kit: pack not built")
	}
	if !kitWarned {
		t.Errorf("warnings %v: want the kit pack one", p.Warnings)
	}
}

package recipe

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The drafts in recipes/ must always be valid.
func TestRepoRecipes(t *testing.T) {
	files, err := filepath.Glob("../../recipes/*/recipe.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recipes found: %v", err)
	}
	for _, f := range files {
		r, err := Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if want := filepath.Base(filepath.Dir(f)); r.Name != want {
			t.Errorf("%s: name %q, want the folder name %q", f, r.Name, want)
		}
	}
}

func TestComfyProfiles(t *testing.T) {
	r, err := Load("../../recipes/comfyui-minimax/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.DefaultProfile(); got != "completo" {
		t.Errorf("default profile %q, want completo", got)
	}
	// Sizes from docs/SERVICES.md §9, in GB (10^9 bytes), rounded.
	for profile, wantGB := range map[string]int64{"basico": 44, "estandar": 71, "completo": 121, "musica": 133} {
		n, err := r.Size(profile)
		if err != nil {
			t.Fatal(err)
		}
		if got := (n + 5e8) / 1e9; got != wantGB {
			t.Errorf("profile %s: %d GB, want %d", profile, got, wantGB)
		}
	}
	arts, _ := r.ProfileArtifacts("basico")
	for _, a := range arts {
		if a.Group != "runtime" && a.Group != "p1" {
			t.Errorf("basico includes %s from group %s", a.ID, a.Group)
		}
	}
	if _, err := r.ProfileArtifacts("nope"); err == nil {
		t.Error("unknown profile accepted")
	}
}

func TestNoProfiles(t *testing.T) {
	r, err := Load("../../recipes/ui-tars/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	arts, err := r.ProfileArtifacts("")
	if err != nil || len(arts) != len(r.Artifacts) {
		t.Fatalf("got %d artifacts, err %v; want all %d", len(arts), err, len(r.Artifacts))
	}
	if r.Services[0].AutostartOf() {
		t.Error("on_demand service should not autostart")
	}
}

func valid() *Recipe {
	return &Recipe{
		Schema:  Schema,
		Name:    "demo",
		Version: "1.0.0",
		Root:    `C:\demo`,
		Profiles: map[string]Profile{
			"min":  {Groups: []string{"a"}, Default: true},
			"full": {Groups: []string{"a", "b"}},
		},
		Artifacts: []Artifact{
			{ID: "one", Group: "a", Path: "models/one.bin", Size: 10, URLs: []string{"https://example.com/one.bin"}},
			{ID: "two", Group: "b", Path: "models/two.bin", Size: 20, URLs: []string{"hf://org/repo/two.bin"}},
			{ID: "base", Path: "base.zip", Size: 5, URLs: []string{"https://example.com/base.zip"}, Unpack: &Unpack{Creates: "base/app.exe"}},
		},
		Files: []File{{Path: "conf/app.yaml", Content: "token: ${secret:hf_token}\n"}},
		Setup: []Step{{Name: "dirs", Shell: "powershell", Run: "mkdir '${root}\\out'; login ${secret:api_key}", Creates: `${root}\out`}},
		Services: []Service{{
			Name:    "app",
			Command: `${root}\app.exe --data ${data} --path ${env:PATH}`,
			Cwd:     "${root}",
			Env:     map[string]string{"KEY": "${data}"},
			Ready:   &Ready{URL: "http://127.0.0.1:9000/health"},
			Expose:  &Expose{Endpoint: "app", Port: 9000, API: "http", Auth: "key"},
		}},
	}
}

func TestValidateOK(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Recipe)
		want string
	}{
		{"schema", func(r *Recipe) { r.Schema = "v0" }, "schema"},
		{"name", func(r *Recipe) { r.Name = "bad name" }, "name"},
		{"version", func(r *Recipe) { r.Version = "1.0" }, "version"},
		{"os", func(r *Recipe) { r.Requires.OS = "beos" }, "requires.os"},
		{"root", func(r *Recipe) { r.Root = "" }, "root: required"},
		{"root self", func(r *Recipe) { r.Root = "${root}/x" }, "root: cannot"},
		{"dup id", func(r *Recipe) { r.Artifacts[1].ID = "one" }, "duplicate id"},
		{"abs path", func(r *Recipe) { r.Artifacts[0].Path = "/etc/passwd" }, "relative"},
		{"drive path", func(r *Recipe) { r.Artifacts[0].Path = "C:/x" }, "relative"},
		{"escape", func(r *Recipe) { r.Artifacts[0].Path = "../x" }, "clean path"},
		{"backslash", func(r *Recipe) { r.Artifacts[0].Path = `models\one.bin` }, "separators"},
		{"stream", func(r *Recipe) { r.Artifacts[0].Path = "models/one.bin:x" }, "Windows does not allow"},
		{"device", func(r *Recipe) { r.Artifacts[0].Path = "models/nul.bin" }, "not a valid file name"},
		{"trailing dot", func(r *Recipe) { r.Artifacts[0].Path = "models./one.bin" }, "not a valid file name"},
		{"relative root", func(r *Recipe) { r.Root = "models" }, "must be absolute"},
		{"secret in service", func(r *Recipe) { r.Services[0].Env["T"] = "${secret:x}" }, "services cannot use secrets"},
		{"dup path", func(r *Recipe) { r.Files[0].Path = "models/one.bin" }, "also used"},
		{"size", func(r *Recipe) { r.Artifacts[0].Size = 0 }, "size: required"},
		{"sha", func(r *Recipe) { r.Artifacts[0].SHA256 = "ABC" }, "sha256"},
		{"url scheme", func(r *Recipe) { r.Artifacts[0].URLs = []string{"ftp://x/y"} }, "scheme"},
		{"hf url", func(r *Recipe) { r.Artifacts[1].URLs = []string{"hf://org"} }, "hf://"},
		{"unpack creates", func(r *Recipe) { r.Artifacts[2].Unpack.Creates = "" }, "unpack.creates: required"},
		{"profile group", func(r *Recipe) { r.Profiles["full"] = Profile{Groups: []string{"zz"}} }, `no artifact has group "zz"`},
		{"two defaults", func(r *Recipe) { r.Profiles["full"] = Profile{Groups: []string{"b"}, Default: true} }, "exactly one"},
		{"groups no profiles", func(r *Recipe) { r.Profiles = nil }, "no profiles"},
		{"step check", func(r *Recipe) { r.Setup[0].Creates = "" }, "unless or creates"},
		{"step shell", func(r *Recipe) { r.Setup[0].Shell = "fish" }, "shell"},
		{"restart", func(r *Recipe) { r.Services[0].Restart = "sometimes" }, "restart"},
		{"ready both", func(r *Recipe) { r.Services[0].Ready.Port = 9000 }, "exactly one of url or port"},
		{"api", func(r *Recipe) { r.Services[0].Expose.API = "grpc" }, "expose.api"},
		{"auth", func(r *Recipe) { r.Services[0].Expose.Auth = "" }, "expose.auth"},
		{"port", func(r *Recipe) { r.Services[0].Expose.Port = 70000 }, "not a port"},
		{"secret name", func(r *Recipe) { r.Files[0].Content = "${secret:}" }, "bad secret"},
		{"on_demand autostart", func(r *Recipe) {
			yes := true
			r.Services[0].OnDemand = &OnDemand{IdleStopSec: 60}
			r.Services[0].Autostart = &yes
		}, "cannot autostart"},
		{"dup port", func(r *Recipe) {
			s := r.Services[0]
			s.Name, s.Expose = "app2", &Expose{Endpoint: "app2", Port: 9000, API: "http", Auth: "none"}
			r.Services = append(r.Services, s)
		}, "9000 is also used"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := valid()
			c.mod(r)
			err := r.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestValidateReportsAll(t *testing.T) {
	r := valid()
	r.Version = "x"
	r.Services[0].Expose.API = "grpc"
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "version") || !strings.Contains(err.Error(), "expose.api") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestParseUnknownField(t *testing.T) {
	data, _ := json.Marshal(valid())
	bad := strings.Replace(string(data), `"root"`, `"rooot"`, 1)
	if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "rooot") {
		t.Fatalf("unknown field accepted: %v", err)
	}
	r, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

func TestSecrets(t *testing.T) {
	if got := valid().Secrets(); !slices.Equal(got, []string{"api_key", "hf_token"}) {
		t.Fatalf("got %v", got)
	}
}

func TestExpand(t *testing.T) {
	r := valid()
	r.Root = `${data}\demo`
	out, err := r.Expand(Vars{Data: `D:\astrea`, Secrets: map[string]string{"hf_token": "T", "api_key": "K"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Root != `D:\astrea\demo` {
		t.Errorf("root %q", out.Root)
	}
	s := out.Services[0]
	if want := `D:\astrea\demo\app.exe --data D:\astrea --path ${env:PATH}`; s.Command != want {
		t.Errorf("command %q, want %q", s.Command, want)
	}
	if s.Cwd != `D:\astrea\demo` || s.Env["KEY"] != `D:\astrea` || !strings.HasSuffix(out.Setup[0].Run, "login K") {
		t.Errorf("cwd %q env %v", s.Cwd, s.Env)
	}
	if out.Files[0].Content != "token: T\n" || out.Setup[0].Creates != `D:\astrea\demo\out` {
		t.Errorf("file %q creates %q", out.Files[0].Content, out.Setup[0].Creates)
	}
	// The original is untouched.
	if r.Setup[0].Run != valid().Setup[0].Run || !strings.HasPrefix(r.Services[0].Command, "${root}") {
		t.Error("Expand modified the original recipe")
	}
}

func TestExpandMissing(t *testing.T) {
	_, err := valid().Expand(Vars{Secrets: map[string]string{"hf_token": "T"}})
	if err == nil || !strings.Contains(err.Error(), "api_key") || !strings.Contains(err.Error(), "${data}") {
		t.Fatalf("got %v", err)
	}
}

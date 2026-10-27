package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEnv "downloads" by writing size bytes of 'x', unpacks by creating the
// unpack.creates file, and records what it was asked to do.
type fakeEnv struct {
	mu       sync.Mutex
	fetched  []string
	resumed  map[string]int64
	unpacked []string
	ran      []string
	services []string
	unless   map[string]string // command → stdout
	failRun  string
	block    chan struct{} // if set, Fetch waits on it (or ctx)
	free     int64
	r        *Recipe
}

func (f *fakeEnv) Check(context.Context, Requires) error { return nil }

func (f *fakeEnv) FreeBytes(string) (int64, error) {
	if f.free == 0 {
		return 1 << 50, nil
	}
	return f.free, nil
}

func (f *fakeEnv) Fetch(ctx context.Context, a Artifact, _ []Source, dest string, progress func(int64)) (string, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	f.fetched = append(f.fetched, a.ID)
	if fi, err := os.Stat(dest + ".part"); err == nil {
		if f.resumed == nil {
			f.resumed = map[string]int64{}
		}
		f.resumed[a.ID] = fi.Size()
	}
	f.mu.Unlock()
	os.Remove(dest + ".part")
	progress(a.Size)
	return "fake", os.WriteFile(dest, []byte(strings.Repeat("x", int(a.Size))), 0o644)
}

func (f *fakeEnv) Unpack(_ context.Context, archive, dir string) error {
	f.mu.Lock()
	f.unpacked = append(f.unpacked, filepath.Base(archive))
	f.mu.Unlock()
	for _, a := range f.r.Artifacts {
		if a.Unpack != nil && strings.HasSuffix(filepath.ToSlash(archive), a.Path) {
			root := strings.TrimSuffix(filepath.ToSlash(archive), a.Path)
			p := filepath.Join(filepath.FromSlash(root), filepath.FromSlash(a.Unpack.Creates))
			os.MkdirAll(filepath.Dir(p), 0o755)
			return os.WriteFile(p, []byte("bin"), 0o644)
		}
	}
	return errors.New("unknown archive")
}

func (f *fakeEnv) Run(_ context.Context, _, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if out, ok := f.unless[command]; ok {
		return out, nil
	}
	f.ran = append(f.ran, command)
	if f.failRun != "" && strings.Contains(command, f.failRun) {
		return "boom output", errors.New("exit status 1")
	}
	return "", nil
}

func (f *fakeEnv) StartService(_ context.Context, recipe string, s Service) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = append(f.services, recipe+"/"+s.Name+": "+s.Command)
	return "started", nil
}

func (f *fakeEnv) StopService(_ context.Context, s Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = append(f.services, "stop "+s.Name)
	return nil
}

func applyRecipe(root string) *Recipe {
	return &Recipe{
		Schema: Schema, Name: "demo", Version: "1.0.0", Root: root,
		Profiles: map[string]Profile{
			"min":  {Groups: []string{"a"}, Default: true},
			"full": {Groups: []string{"a", "b"}},
		},
		Artifacts: []Artifact{
			{ID: "model", Group: "a", Path: "models/m.bin", Size: 8, URLs: []string{"https://x/m"}},
			{ID: "app", Group: "a", Path: "dist/app.zip", Size: 4, URLs: []string{"https://x/app"}, Unpack: &Unpack{To: "app", Creates: "app/run.exe"}},
			{ID: "extra", Group: "b", Path: "models/e.bin", Size: 3, URLs: []string{"https://x/e"}},
		},
		Files: []File{{Path: "conf/app.yaml", Content: "root: ${root}\nkey: ${secret:k}\n"}},
		Setup: []Step{
			{Name: "fw", Run: "add-firewall", Unless: "has-firewall"},
			{Name: "dirs", Run: "make-dirs", Creates: "${root}/out"},
		},
		Services: []Service{{Name: "app", Command: "${root}/app/run.exe", Expose: &Expose{Endpoint: "app", Port: 9000, API: "http", Auth: "none"}}},
	}
}

func newInstaller(t *testing.T, env *fakeEnv) (*Installer, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "installed.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Installer{Env: env, Store: st, Data: dir}, filepath.Join(dir, "root")
}

func raw(r *Recipe) json.RawMessage {
	b, _ := json.Marshal(r)
	return b
}

func waitDone(t *testing.T, in *Installer, name string) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := in.Wait(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestApplyFresh(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	if _, err := in.Apply(ApplyRequest{Recipe: raw(r)}); err == nil || !strings.Contains(err.Error(), "missing secrets: k") {
		t.Fatalf("want missing secret error, got %v", err)
	}
	if _, err := in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, in, "demo")
	if st.State != StateInstalled {
		t.Fatalf("state %s: %s\n%s", st.State, st.Error, strings.Join(st.Log, "\n"))
	}
	if strings.Join(env.fetched, ",") != "model,app" || strings.Join(env.unpacked, ",") != "app.zip" {
		t.Errorf("fetched %v unpacked %v", env.fetched, env.unpacked)
	}
	if strings.Join(env.ran, ",") != "has-firewall,add-firewall,make-dirs" {
		// has-firewall printed nothing, so the step ran.
		t.Errorf("ran %v", env.ran)
	}
	conf, _ := os.ReadFile(filepath.Join(root, "conf", "app.yaml"))
	if string(conf) != "root: "+root+"\nkey: S\n" {
		t.Errorf("conf %q", conf)
	}
	if len(env.services) != 1 || env.services[0] != "demo/app: "+root+"/app/run.exe" {
		t.Errorf("services %v", env.services)
	}
	if st.Done != st.Total || st.Total != 12 || st.Percent != 100 {
		t.Errorf("progress %d/%d %.1f%%", st.Done, st.Total, st.Percent)
	}
	// installed.json has it, without the secret.
	it, _ := in.Store.Get("demo")
	if it.State != StateInstalled || it.Profile != "min" || strings.Contains(string(it.Recipe), `"S"`) {
		t.Errorf("stored %+v", it)
	}
	data, _ := os.ReadFile(in.Store.path)
	if strings.Contains(string(data), "key: S") {
		t.Error("secret written to installed.json")
	}
}

// Applying again on a machine that has everything downloads nothing and skips
// steps that are done.
func TestApplyAgainIsNoop(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	req := ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}}
	in.Apply(req)
	waitDone(t, in, "demo")
	os.MkdirAll(filepath.Join(root, "out"), 0o755)
	// The archive can go once unpacked: it is not needed again.
	os.Remove(filepath.Join(root, "dist", "app.zip"))
	env.fetched, env.unpacked, env.ran = nil, nil, nil
	env.unless = map[string]string{"has-firewall": "rule found"}
	if _, err := in.Apply(req); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, in, "demo")
	if st.State != StateInstalled || len(env.fetched)+len(env.unpacked)+len(env.ran) != 0 {
		t.Fatalf("state %s fetched %v unpacked %v ran %v", st.State, env.fetched, env.unpacked, env.ran)
	}
	if st.Total != 0 {
		t.Errorf("total %d, want 0", st.Total)
	}
}

func TestApplyResumesPartAndReplacesWrong(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	r.Files = nil
	env.r = r
	os.MkdirAll(filepath.Join(root, "models"), 0o755)
	os.WriteFile(filepath.Join(root, "models", "m.bin.part"), []byte("xxx"), 0o644)
	os.WriteFile(filepath.Join(root, "models", "e.bin"), []byte("too long"), 0o644)
	if _, err := in.Apply(ApplyRequest{Recipe: raw(r), Profile: "full"}); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, in, "demo")
	if st.State != StateInstalled {
		t.Fatalf("%s: %s", st.State, st.Error)
	}
	if env.resumed["model"] != 3 {
		t.Errorf("model not resumed from its .part: %v", env.resumed)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "models", "e.bin")); string(b) != "xxx" {
		t.Errorf("wrong-size file not replaced: %q", b)
	}
	// 5 left of model + 4 app + 3 extra.
	if st.Total != 12 || st.Done != 12 {
		t.Errorf("progress %d/%d", st.Done, st.Total)
	}
}

func TestApplyStepFails(t *testing.T) {
	env := &fakeEnv{failRun: "make-dirs"}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}})
	st := waitDone(t, in, "demo")
	if st.State != StateFailed || st.Phase != PhaseSetup || !strings.Contains(st.Error, "boom output") {
		t.Fatalf("got %s/%s: %s", st.State, st.Phase, st.Error)
	}
	if len(env.services) != 0 {
		t.Error("services started after a failed step")
	}
	if it, _ := in.Store.Get("demo"); it.State != StateFailed || it.Error == "" {
		t.Errorf("stored %+v", it)
	}
}

func TestApplyCancelAndBusy(t *testing.T) {
	env := &fakeEnv{block: make(chan struct{})}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	req := ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}}
	if _, err := in.Apply(req); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(req); err == nil || !strings.Contains(err.Error(), "already being installed") {
		t.Fatalf("second apply: %v", err)
	}
	st, _ := in.Cancel("demo")
	if st.State != StateCanceled {
		t.Fatalf("state %s", st.State)
	}
}

func TestApplyNoSpace(t *testing.T) {
	env := &fakeEnv{free: 5}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}})
	if st := waitDone(t, in, "demo"); st.State != StateFailed || !strings.Contains(st.Error, "not enough disk space") {
		t.Fatalf("%s: %s", st.State, st.Error)
	}
}

func TestApplyPortConflict(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}})
	waitDone(t, in, "demo")
	other := applyRecipe(root + "2")
	other.Name = "other"
	if _, err := in.Apply(ApplyRequest{Recipe: raw(other), Secrets: map[string]string{"k": "S"}}); err == nil ||
		!strings.Contains(err.Error(), "port 9000") || !strings.Contains(err.Error(), `endpoint "app"`) ||
		!strings.Contains(err.Error(), `service "app"`) {
		t.Fatalf("got %v", err)
	}
}

// A restart mid-install: the next agent resumes it from installed.json.
func TestResume(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	r.Files = nil // no secrets
	env.r = r
	in.Store.Update("demo", func(it *Installed) { it.State, it.Recipe, it.Version = StateInstalling, raw(r), r.Version })
	if st, _ := in.Status("demo"); st.State != StateFailed || !strings.Contains(st.Error, "interrupted") {
		t.Errorf("before resume: %+v", st)
	}
	if got := in.Resume(); len(got) != 1 {
		t.Fatalf("resumed %v", got)
	}
	if st := waitDone(t, in, "demo"); st.State != StateInstalled {
		t.Fatalf("%s: %s", st.State, st.Error)
	}
	// With secrets it cannot resume alone.
	r2 := applyRecipe(root + "2")
	r2.Name = "sec"
	r2.Services = nil
	in.Store.Update("sec", func(it *Installed) { it.State, it.Recipe, it.NeedsSecrets = StateInstalling, raw(r2), true })
	in.Resume()
	if it, _ := in.Store.Get("sec"); it.State != StateFailed || !strings.Contains(it.Error, "secrets") {
		t.Errorf("sec: %+v", it)
	}
}

func TestStoreReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "installed.json")
	s, _ := OpenStore(p)
	s.Update("a", func(it *Installed) { it.State = StateInstalled })
	s2, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if it, ok := s2.Get("a"); !ok || it.State != StateInstalled {
		t.Fatalf("reloaded %+v", it)
	}
}

func TestRemove(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}})
	waitDone(t, in, "demo")
	os.WriteFile(filepath.Join(root, "keep.txt"), []byte("mine"), 0o644)
	if _, err := in.Remove(context.Background(), "demo", true); err != nil {
		t.Fatal(err)
	}
	if env.services[len(env.services)-1] != "stop app" {
		t.Errorf("services %v", env.services)
	}
	for _, p := range []string{"models/m.bin", "dist/app.zip", "app", "conf/app.yaml"} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Error("purge deleted a file that is not the recipe's")
	}
	if _, ok := in.Store.Get("demo"); ok {
		t.Error("still in installed.json")
	}
	if _, err := in.Remove(context.Background(), "demo", false); err == nil {
		t.Error("removing twice should fail")
	}
}

// A failed step's error is stored in installed.json: never with a secret.
func TestApplyStepFailsHidesSecret(t *testing.T) {
	env := &fakeEnv{failRun: "make-dirs"}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	r.Setup[1].Run = "make-dirs --key ${secret:k}"
	env.r = r
	in.Apply(ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "hunter2"}})
	st := waitDone(t, in, "demo")
	it, _ := in.Store.Get("demo")
	if st.State != StateFailed || strings.Contains(st.Error, "hunter2") || strings.Contains(it.Error, "hunter2") {
		t.Fatalf("%s: %q / %q", st.State, st.Error, it.Error)
	}
}

// An unpack that died half way leaves its marker: unpack.creates is not
// trusted and the next apply unpacks again.
func TestApplyUnpackInterrupted(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	req := ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}}
	in.Apply(req)
	waitDone(t, in, "demo")
	marker := filepath.Join(root, UnpackMarker("app"))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker left after a good unpack")
	}
	os.WriteFile(marker, nil, 0o644)
	env.unpacked = nil
	in.Apply(req)
	if st := waitDone(t, in, "demo"); st.State != StateInstalled || strings.Join(env.unpacked, ",") != "app.zip" {
		t.Fatalf("%s: unpacked %v", st.State, env.unpacked)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker left")
	}
}

// While Remove deletes, Apply cannot start (it would recreate the entry over
// files being purged).
func TestRemoveBlocksApply(t *testing.T) {
	env := &fakeEnv{}
	in, root := newInstaller(t, env)
	r := applyRecipe(root)
	env.r = r
	req := ApplyRequest{Recipe: raw(r), Secrets: map[string]string{"k": "S"}}
	in.Apply(req)
	waitDone(t, in, "demo")
	in.mu.Lock()
	hold := &job{cancel: func() {}, done: make(chan struct{}), removing: true}
	in.jobs["demo"] = hold
	in.mu.Unlock()
	if _, err := in.Apply(req); err == nil || !strings.Contains(err.Error(), "being removed") {
		t.Fatalf("apply during remove: %v", err)
	}
	if _, err := in.Remove(context.Background(), "demo", false); err == nil || !strings.Contains(err.Error(), "already being removed") {
		t.Fatalf("second remove: %v", err)
	}
	close(hold.done)
	if _, err := in.Remove(context.Background(), "demo", false); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(req); err != nil {
		t.Fatalf("apply after remove: %v", err)
	}
	waitDone(t, in, "demo")
}

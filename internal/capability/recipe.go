package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"github.com/Solar2004/Astrea/internal/download"
	"github.com/Solar2004/Astrea/internal/endpoint"
	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/recipe"
	"github.com/Solar2004/Astrea/internal/shell"
	"github.com/Solar2004/Astrea/internal/supervise"
)

// RecipeEnv is the agent side of recipe.Env: downloads through the download
// manager, services through the supervisor, endpoints through /dyn/.
type RecipeEnv struct {
	Dloads *download.Manager
	Svcs   *supervise.Services
	Dyn    *endpoint.Manager
	// ServiceReadySec is how long StartService waits for a new service to
	// answer its ready probe (default 180: ComfyUI loads for about a minute).
	ServiceReadySec int
}

func (e *RecipeEnv) Check(ctx context.Context, req recipe.Requires) error {
	if req.OS != "" && req.OS != runtime.GOOS {
		return fmt.Errorf("needs %s, this is %s", req.OS, runtime.GOOS)
	}
	if req.GPU == "" && req.VRAMMB == 0 {
		return nil
	}
	gpus := nvidiaSMI(ctx)
	if len(gpus) == 0 {
		return errors.New("needs an NVIDIA GPU (nvidia-smi found none)")
	}
	if req.VRAMMB > 0 {
		best := uint64(0)
		for _, g := range gpus {
			best = max(best, g.VRAMTotal)
		}
		// nvidia-smi reports a bit less than the marketing size (16 GB cards
		// say 16376 MiB); recipes state what they really need.
		if best < uint64(req.VRAMMB)<<20 {
			return fmt.Errorf("needs %d MB of VRAM, the GPU has %d MB", req.VRAMMB, best>>20)
		}
	}
	return nil
}

// FreeBytes measures the disk of dir's nearest existing ancestor: before the
// first install root does not exist, and on Linux the volume name of a path
// is always empty, so "/" would be measured instead of its mount.
func (e *RecipeEnv) FreeBytes(dir string) (int64, error) {
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	u, err := disk.Usage(dir)
	if err != nil {
		return 0, err
	}
	return int64(u.Free), nil
}

// Fetch tries each source machine, then each mirror, until one works. A
// failed source leaves its .part behind, so the next one resumes it, unless
// the artifact has no sha256: then nothing would catch two sources with
// different bytes stitched together, so the next one starts over.
func (e *RecipeEnv) Fetch(ctx context.Context, a recipe.Artifact, sources []recipe.Source, dest string, progress func(int64)) (string, error) {
	type try struct {
		label   string
		url     string
		headers map[string]string
	}
	var tries []try
	for _, s := range sources {
		u := strings.TrimSuffix(s.URL, "/") + "/" + escapePath(a.Path)
		var h map[string]string
		if s.Token != "" {
			h = map[string]string{"Authorization": "Bearer " + s.Token}
		}
		label := s.Name
		if label == "" {
			label = s.URL
		}
		tries = append(tries, try{label, u, h})
	}
	for _, u := range a.URLs {
		tries = append(tries, try{u, u, nil})
	}
	if len(tries) == 0 {
		return "", errors.New("no source has it: it has no mirror and no machine with it was given")
	}
	var errs []string
	for i, t := range tries {
		// Two tries per source while there are others to fall back on: a
		// dead mirror costs ~2 s instead of ~30 s. The last one gets the
		// usual five (the school's DNS fails now and then).
		attempts := 2
		if i == len(tries)-1 {
			attempts = 0
		}
		err := e.fetchOne(ctx, t.url, t.headers, attempts, a, dest, progress)
		if err == nil {
			if len(errs) > 0 {
				return t.label + " (failed before: " + strings.Join(errs, "; ") + ")", nil
			}
			return t.label, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		errs = append(errs, t.label+": "+err.Error())
		if a.SHA256 == "" {
			os.Remove(dest + ".part")
		}
	}
	return "", fmt.Errorf("every source failed: %s", strings.Join(errs, "; "))
}

func (e *RecipeEnv) fetchOne(ctx context.Context, url string, headers map[string]string, attempts int, a recipe.Artifact, dest string, progress func(int64)) error {
	st, err := e.Dloads.Start(download.Request{URL: url, Dest: dest, SHA256: a.SHA256, Headers: headers, Attempts: attempts})
	if err != nil {
		return err
	}
	for {
		s, err := e.Dloads.Get(ctx, st.ID, 2*time.Second)
		if ctx.Err() != nil {
			// The download manager runs on its own context: stop it, or it
			// keeps writing after the install was canceled.
			e.Dloads.Cancel(st.ID)
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		progress(s.Bytes)
		switch s.State {
		case download.Done:
			if a.Size > 0 {
				fi, err := os.Stat(dest)
				if err != nil {
					return err
				}
				if fi.Size() != a.Size {
					os.Remove(dest)
					return fmt.Errorf("got %d bytes, the recipe says %d", fi.Size(), a.Size)
				}
			}
			return nil
		case download.Failed:
			return errors.New(s.Error)
		case download.Canceled:
			return errors.New("canceled")
		}
	}
}

// escapePath escapes each segment of a recipe path for a URL: '#' or '?' in a
// file name would cut it otherwise.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Unpack extracts zip archives itself and everything else (7z, tar…) with
// tar, which on Windows 10+ is bsdtar (libarchive).
func (e *RecipeEnv) Unpack(ctx context.Context, archive, dir string) error {
	if strings.EqualFold(filepath.Ext(archive), ".zip") {
		return unzip(archive, dir)
	}
	cmd := exec.Command("tar", "-xf", archive, "-C", dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := runTree(ctx, cmd); err != nil {
		return fmt.Errorf("tar: %v: %s", err, clipStr(strings.TrimSpace(out.String()), 400))
	}
	return nil
}

// runTree runs cmd and, if ctx ends first, kills it with everything it
// started. exec.CommandContext kills only the shell: on Windows pip or an
// installer it spawned would keep running and hold the output pipes open, so
// Wait (and the install's cancel) would hang until they exit.
func runTree(ctx context.Context, cmd *exec.Cmd) error {
	procutil.NoConsole(cmd)
	procutil.PrepareTree(cmd)
	cmd.WaitDelay = 5 * time.Second // pipes a stray grandchild keeps open
	if err := cmd.Start(); err != nil {
		return err
	}
	tree, terr := procutil.AttachTree(cmd)
	exited := make(chan struct{})
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		close(exited)
		waited <- err
	}()
	select {
	case err := <-waited:
		if terr == nil {
			tree.Close()
		}
		return err
	case <-ctx.Done():
		if terr == nil {
			tree.Kill(3*time.Second, exited)
			tree.Close()
		} else {
			cmd.Process.Kill()
		}
		<-waited
		return ctx.Err()
	}
}

func (e *RecipeEnv) Run(ctx context.Context, sh, command string) (string, error) {
	name, args, err := shell.Command(sh, command)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = runTree(ctx, cmd)
	if err != nil {
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err
	}
	return stdout.String(), nil
}

// StartService defines the service and publishes its endpoint. A service
// that is already running with the same definition is left alone, so
// adopting a machine does not restart (and interrupt) what it is doing.
func (e *RecipeEnv) StartService(ctx context.Context, recipeName string, s recipe.Service) (string, error) {
	spec := supervise.ServiceSpec{
		Name:      s.Name,
		Command:   supervise.Command{Command: s.Command, Shell: s.Shell, Cwd: s.Cwd, Env: s.Env},
		Restart:   s.Restart,
		Autostart: s.AutostartOf(),
	}
	if spec.Restart == "" {
		spec.Restart = supervise.RestartOnFailure
	}
	if s.Ready != nil {
		spec.Ready = &supervise.Ready{URL: s.Ready.URL, Port: s.Ready.Port}
	}
	if s.Expose != nil {
		spec.Expose = &supervise.Expose{Endpoint: s.Expose.Endpoint, Port: s.Expose.Port}
	}

	note := ""
	cur, err := e.Svcs.Status(s.Name)
	if err == nil && sameService(cur.ServiceSpec, spec) && cur.State == supervise.SvcRunning {
		note = "unchanged, already running (pid " + strconv.Itoa(cur.PID) + ")"
	} else {
		if _, err := e.Svcs.Start(spec); err != nil {
			return "", err
		}
		wait := e.ServiceReadySec
		if wait <= 0 {
			wait = 180
		}
		if spec.Ready == nil {
			note = "started"
		} else if ok, why := e.Svcs.WaitReady(ctx, s.Name, time.Duration(wait)*time.Second); ok {
			note = "started and ready"
		} else {
			st, _ := e.Svcs.Status(s.Name)
			if st.State != supervise.SvcRunning && st.State != supervise.SvcStarting {
				tail := ""
				if lc, err := e.Svcs.Log(s.Name, 0, 15, 0); err == nil {
					tail = "; log tail: " + clipStr(strings.TrimSpace(lc.Data), 800)
				}
				return "", fmt.Errorf("did not start (%s, %s)%s", st.State, why, tail)
			}
			note = "started, not ready yet (" + why + "); it keeps starting in the background"
		}
	}
	if s.Expose != nil && e.Dyn != nil {
		target := "http://127.0.0.1:" + strconv.Itoa(s.Expose.Port)
		if err := e.Dyn.Create(&endpoint.Spec{Name: s.Expose.Endpoint, Kind: endpoint.KindProxy, Target: target}); err != nil {
			return "", fmt.Errorf("expose: %w", err)
		}
		note += "; published at /dyn/" + s.Expose.Endpoint
	}
	return note, nil
}

func (e *RecipeEnv) StopService(_ context.Context, s recipe.Service) error {
	if _, err := e.Svcs.Stop(s.Name, true); err != nil && !strings.Contains(err.Error(), "no service") {
		return err
	}
	if s.Expose != nil && e.Dyn != nil {
		e.Dyn.Delete(s.Expose.Endpoint)
	}
	return nil
}

// sameService reports whether a running service already runs what spec
// describes. Only what changes the process counts: ready and expose do not
// (the endpoint is published separately), so adopting a service started by
// hand without expose does not restart it.
func sameService(cur, spec supervise.ServiceSpec) bool {
	if len(cur.Env) == 0 {
		cur.Env = nil
	}
	if len(spec.Env) == 0 {
		spec.Env = nil
	}
	return reflect.DeepEqual(cur.Command, spec.Command) && cur.Restart == spec.Restart && cur.Autostart == spec.Autostart
}

// RecipeApply installs, updates or adopts a recipe.
type RecipeApply struct{ In *recipe.Installer }

func (RecipeApply) Name() string { return "recipe.apply" }

func (RecipeApply) Describe() Descriptor {
	return Descriptor{
		Name: "recipe.apply",
		Description: "Install a service recipe on this machine (or update it, or adopt what is already there): downloads only the artifacts " +
			"that are missing or wrong (resuming .part files), unpacks, writes config files, runs the setup steps that are not done yet " +
			"and starts the services, publishing their endpoints under /dyn/. A service already running with the same definition is not restarted. " +
			"It runs in the background: it returns at once (or after wait_sec) and recipe.status follows it. See docs/SERVICES.md for the format.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"recipe": {"type": "object", "description": "The recipe (astrea.recipe/v1)."},
				"profile": {"type": "string", "description": "Which artifact profile; default the recipe's default."},
				"secrets": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Values for ${secret:name}. Never stored."},
				"verify": {"type": "boolean", "description": "sha256 the artifacts already on disk before trusting them (minutes for big models). Default false: size decides."},
				"sources": {"type": "array", "items": {"type": "object", "properties": {"name": {"type": "string"}, "url": {"type": "string"}, "token": {"type": "string"}}, "required": ["url"]},
					"description": "Other machines to copy from first: url is http://<agent>:7700/files/<recipe>/ and token that agent's token."},
				"wait_sec": {"type": "integer", "description": "Wait up to this long for it to finish (max 120). Default 0."}
			},
			"required": ["recipe"]
		}`),
	}
}

func (c RecipeApply) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		recipe.ApplyRequest
		WaitSec int `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if len(req.Recipe) == 0 {
		return nil, badRequestMsg("recipe is required")
	}
	st, err := c.In.Apply(req.ApplyRequest)
	if err != nil {
		return nil, badRequest(err)
	}
	if req.WaitSec > 0 {
		wctx, cancel := context.WithTimeout(ctx, waitDur(req.WaitSec))
		defer cancel()
		if ws, err := c.In.Wait(wctx, st.Name); err == nil {
			st = ws
		}
	}
	return st, nil
}

// RecipePlan shows what recipe.apply would do, without doing it.
type RecipePlan struct {
	In *recipe.Installer
}

func (RecipePlan) Name() string { return "recipe.plan" }

func (RecipePlan) Describe() Descriptor {
	return Descriptor{
		Name: "recipe.plan",
		Description: "Dry run of recipe.apply: compares the recipe with this machine's disk and says, per artifact, whether it is present, " +
			"missing, partial (.part), the wrong size or hash, or already unpacked; which config files differ; how many bytes are left to download; " +
			"and which artifacts of other profiles are on disk. Changes nothing.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"recipe": {"type": "object"},
				"profile": {"type": "string"},
				"secrets": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Only needed to compare config files that use secrets."},
				"verify": {"type": "boolean", "description": "Also sha256 the files that have the right size (slow)."}
			},
			"required": ["recipe"]
		}`),
	}
}

func (c RecipePlan) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req recipe.ApplyRequest
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	r, err := recipe.Parse(req.Recipe)
	if err != nil {
		return nil, badRequest(err)
	}
	if err := r.Validate(); err != nil {
		return nil, badRequest(err)
	}
	out := map[string]any{"recipe": r.Name, "version": r.Version}
	if err := c.In.Env.Check(ctx, r.Requires); err != nil {
		out["applies"] = false
		out["reason"] = err.Error()
		return out, nil
	}
	secrets := req.Secrets
	if secrets == nil {
		secrets = map[string]string{}
	}
	// Unknown secrets compare as empty: the file shows as "differs".
	for _, s := range r.Secrets() {
		if _, ok := secrets[s]; !ok {
			secrets[s] = ""
		}
	}
	x, err := r.Expand(recipe.Vars{Data: c.In.Data, Secrets: secrets})
	if err != nil {
		return nil, badRequest(err)
	}
	if !filepath.IsAbs(x.Root) {
		return nil, badRequestMsg(fmt.Sprintf("root %q is not an absolute path on this machine", x.Root))
	}
	plan, err := recipe.MakePlan(x, os.DirFS(x.Root), recipe.PlanOptions{Profile: req.Profile, Verify: req.Verify, Ctx: ctx})
	if err != nil {
		return nil, badRequest(err)
	}
	out["applies"] = true
	out["root"] = x.Root
	out["plan"] = plan
	out["done"] = plan.Done()
	if free, err := c.In.Env.FreeBytes(x.Root); err == nil {
		out["free_bytes"] = free
	}
	return out, nil
}

// RecipeStatus reports installs: one recipe, or everything installed here.
type RecipeStatus struct{ In *recipe.Installer }

func (RecipeStatus) Name() string { return "recipe.status" }

func (RecipeStatus) Describe() Descriptor {
	return Descriptor{
		Name: "recipe.status",
		Description: "Progress of a recipe install (phase, current artifact or step, bytes, %, speed, ETA, log) or, without name, " +
			"every recipe installed on this machine with its version, profile and state.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"wait_sec": {"type": "integer", "description": "With name: wait up to this long (max 120) for the install to end."}
			}
		}`),
	}
}

func (c RecipeStatus) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name    string `json:"name"`
		WaitSec int    `json:"wait_sec"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	if req.Name == "" {
		list := []recipe.Status{}
		for _, it := range c.In.Store.List() {
			if st, err := c.In.Status(it.Name); err == nil {
				st.Log, st.Plan = nil, nil
				list = append(list, st)
			}
		}
		return map[string]any{"installed": list}, nil
	}
	if req.WaitSec > 0 {
		wctx, cancel := context.WithTimeout(ctx, waitDur(req.WaitSec))
		defer cancel()
		st, err := c.In.Wait(wctx, req.Name)
		if err != nil {
			return nil, badRequest(err)
		}
		return st, nil
	}
	st, err := c.In.Status(req.Name)
	if err != nil {
		return nil, badRequest(err)
	}
	return st, nil
}

// RecipeCancel stops a running install.
type RecipeCancel struct{ In *recipe.Installer }

func (RecipeCancel) Name() string { return "recipe.cancel" }

func (RecipeCancel) Describe() Descriptor {
	return Descriptor{
		Name:        "recipe.cancel",
		Description: "Stop a running recipe install. Finished downloads stay and the one in progress keeps its .part, so recipe.apply resumes.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}`),
	}
}

func (c RecipeCancel) Invoke(_ context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	st, err := c.In.Cancel(req.Name)
	if err != nil {
		return nil, badRequest(err)
	}
	return st, nil
}

// RecipeRemove uninstalls a recipe.
type RecipeRemove struct{ In *recipe.Installer }

func (RecipeRemove) Name() string { return "recipe.remove" }

func (RecipeRemove) Describe() Descriptor {
	return Descriptor{
		Name: "recipe.remove",
		Description: "Uninstall a recipe from this machine: stops and forgets its services and endpoints. With purge it also deletes " +
			"its artifacts (models…), its config files and the folders it unpacked into; never other files under its root.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {"name": {"type": "string"}, "purge": {"type": "boolean"}}, "required": ["name"]}`),
	}
}

func (c RecipeRemove) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		Name  string `json:"name"`
		Purge bool   `json:"purge"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	done, err := c.In.Remove(ctx, req.Name, req.Purge)
	if err != nil {
		return nil, badRequest(fmt.Errorf("%w (done so far: %s)", err, strings.Join(done, "; ")))
	}
	return map[string]any{"removed": req.Name, "done": done}, nil
}

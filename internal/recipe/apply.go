package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Env is what applying a recipe needs from the machine. The agent implements
// it; tests use a fake. Paths are absolute OS paths.
type Env interface {
	// Check reports why this machine cannot have the recipe (nil if it can).
	Check(ctx context.Context, req Requires) error
	// FreeBytes is the free space on the disk that holds dir.
	FreeBytes(dir string) (int64, error)
	// Fetch downloads the artifact to dest, resuming dest+".part", and
	// verifies size and sha256 when the artifact has them. It tries the
	// other machines in sources first (LAN is faster), then the mirrors.
	// progress gets the size of the file so far, including what a resumed
	// .part already had. It returns where the file came from (and which
	// sources failed first), for the install log.
	Fetch(ctx context.Context, a Artifact, sources []Source, dest string, progress func(int64)) (string, error)
	// Unpack extracts archive into dir.
	Unpack(ctx context.Context, archive, dir string) error
	// Run runs a setup command and returns its standard output. A non-zero
	// exit is an error.
	Run(ctx context.Context, shell, command string) (string, error)
	// StartService defines and starts (or leaves alone, if unchanged) one
	// service of the recipe and publishes its endpoint. It returns a short
	// note for the install log.
	StartService(ctx context.Context, recipe string, s Service) (string, error)
	// StopService stops and forgets a service and removes its endpoint.
	StopService(ctx context.Context, s Service) error
}

// Install phases, in order.
const (
	PhasePlan     = "plan"
	PhaseFetch    = "fetch"
	PhaseUnpack   = "unpack"
	PhaseFiles    = "files"
	PhaseSetup    = "setup"
	PhaseServices = "services"
)

// Status is the live progress of an install, or the last known state from
// installed.json when no install is running.
type Status struct {
	Name       string    `json:"name"`
	Version    string    `json:"version"`
	Profile    string    `json:"profile,omitempty"`
	State      string    `json:"state"`
	Phase      string    `json:"phase,omitempty"`
	Current    string    `json:"current,omitempty"` // artifact, step or service
	Done       int64     `json:"done_bytes,omitempty"`
	Total      int64     `json:"total_bytes,omitempty"` // bytes to download
	Percent    float64   `json:"percent,omitempty"`
	SpeedBps   float64   `json:"speed_bps,omitempty"`
	ETASec     float64   `json:"eta_sec,omitempty"`
	Plan       *Plan     `json:"plan,omitempty"`
	Log        []string  `json:"log,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// ApplyRequest asks to install (or update, or adopt) a recipe.
type ApplyRequest struct {
	Recipe  json.RawMessage   `json:"recipe"`
	Profile string            `json:"profile,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// Verify hashes the artifacts that are already on disk before trusting
	// them. Slow on big models (minutes); size alone is the default.
	Verify bool `json:"verify,omitempty"`
	// Sources are other machines that have the recipe installed. They are
	// used only for this run (never stored: they carry tokens).
	Sources []Source `json:"sources,omitempty"`
}

// Source is another machine serving its artifacts at URL (its
// /files/<recipe>/ route), with the token that route needs.
type Source struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// Installer runs installs in the background, one per recipe at a time.
type Installer struct {
	Env   Env
	Store *Store
	// Data is the agent's data directory, ${data} in recipes.
	Data string

	mu   sync.Mutex
	jobs map[string]*job
}

type job struct {
	cancel context.CancelFunc
	done   chan struct{}
	// removing marks the placeholder Remove puts in jobs while it deletes,
	// so an Apply cannot start meanwhile.
	removing bool

	mu       sync.Mutex
	st       Status
	fetched  int64 // bytes of finished artifacts in this run
	lastAt   time.Time
	lastDone int64
}

const maxLog = 100

func (j *job) set(fn func(*Status)) {
	j.mu.Lock()
	fn(&j.st)
	j.mu.Unlock()
}

func (j *job) logf(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	j.set(func(s *Status) {
		s.Log = append(s.Log, line)
		if len(s.Log) > maxLog {
			s.Log = s.Log[len(s.Log)-maxLog:]
		}
	})
}

func (j *job) status() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := j.st
	st.Log = append([]string(nil), j.st.Log...)
	return st
}

// progress records the bytes of the artifact being fetched; done counts
// only that artifact.
func (j *job) progress(done int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	total := j.fetched + done
	if !j.lastAt.IsZero() {
		if dt := now.Sub(j.lastAt).Seconds(); dt >= 1 {
			speed := float64(total-j.lastDone) / dt
			if j.st.SpeedBps > 0 {
				speed = 0.7*j.st.SpeedBps + 0.3*speed
			}
			j.st.SpeedBps = speed
			j.lastAt, j.lastDone = now, total
		}
	} else {
		j.lastAt, j.lastDone = now, total
	}
	j.st.Done = total
	if j.st.Total > 0 {
		j.st.Percent = min(100, float64(int64(float64(total)*1000/float64(j.st.Total)))/10)
		if j.st.SpeedBps > 0 {
			j.st.ETASec = float64(int64(float64(max(0, j.st.Total-total)) / j.st.SpeedBps))
		}
	}
}

// Apply validates the request and starts the install in the background. It
// returns at once; follow it with Status.
func (in *Installer) Apply(req ApplyRequest) (Status, error) {
	r, err := Parse(req.Recipe)
	if err != nil {
		return Status{}, err
	}
	if err := r.Validate(); err != nil {
		return Status{}, err
	}
	for _, s := range r.Services {
		if s.OnDemand != nil {
			return Status{}, fmt.Errorf("service %q: on_demand services are not supported yet", s.Name)
		}
	}
	if missing := missingSecrets(r, req.Secrets); len(missing) > 0 {
		return Status{}, fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	if _, err := r.ProfileArtifacts(req.Profile); err != nil {
		return Status{}, err
	}
	if _, err := r.RootFor(in.Data); err != nil {
		return Status{}, err
	}
	raw, err := json.Marshal(r) // normalized: no unknown fields, no secrets values
	if err != nil {
		return Status{}, err
	}
	profile := req.Profile
	if profile == "" {
		profile = r.DefaultProfile()
	}

	in.mu.Lock()
	if in.jobs == nil {
		in.jobs = map[string]*job{}
	}
	if j := in.jobs[r.Name]; j != nil {
		select {
		case <-j.done:
		default:
			in.mu.Unlock()
			if j.removing {
				return Status{}, fmt.Errorf("%s is being removed; wait", r.Name)
			}
			return Status{}, fmt.Errorf("%s is already being installed; cancel it first or wait", r.Name)
		}
	}
	// Checked and recorded under the lock: two recipes applied at once with
	// the same port would both pass otherwise.
	if err := in.conflicts(r); err != nil {
		in.mu.Unlock()
		return Status{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{cancel: cancel, done: make(chan struct{})}
	j.st = Status{Name: r.Name, Version: r.Version, Profile: profile, State: StateInstalling, Phase: PhasePlan, StartedAt: time.Now().UTC()}
	in.jobs[r.Name] = j
	in.Store.Update(r.Name, func(it *Installed) {
		it.Version, it.Profile, it.Recipe = r.Version, profile, raw
		it.State, it.Phase, it.Error = StateInstalling, PhasePlan, ""
		it.StartedAt = j.st.StartedAt
		it.NeedsSecrets = len(r.Secrets()) > 0
	})
	in.mu.Unlock()
	go in.run(ctx, j, r, profile, req)
	return j.status(), nil
}

// conflicts refuses a recipe whose services, endpoints or ports are already
// used by another recipe recorded on this machine (docs/SERVICES.md §7.9),
// failed and canceled ones included: their services may be running. Called
// with in.mu held.
func (in *Installer) conflicts(r *Recipe) error {
	mine := map[string]bool{}
	ports := map[int]bool{}
	names := map[string]bool{}
	for _, s := range r.Services {
		names[s.Name] = true
		if s.Expose != nil {
			mine[s.Expose.Endpoint] = true
			ports[s.Expose.Port] = true
		}
	}
	var errs []error
	for _, it := range in.Store.List() {
		if it.Name == r.Name {
			continue
		}
		other, err := Parse(it.Recipe)
		if err != nil {
			continue
		}
		hint := ""
		if it.State != StateInstalled {
			hint = " (" + it.State + "; recipe.remove it if it is not wanted)"
		}
		for _, s := range other.Services {
			if names[s.Name] {
				errs = append(errs, fmt.Errorf("service %q is already defined by recipe %s%s", s.Name, it.Name, hint))
			}
			if s.Expose == nil {
				continue
			}
			if mine[s.Expose.Endpoint] {
				errs = append(errs, fmt.Errorf("endpoint %q is already published by recipe %s%s", s.Expose.Endpoint, it.Name, hint))
			}
			if ports[s.Expose.Port] {
				errs = append(errs, fmt.Errorf("port %d is already used by recipe %s%s", s.Expose.Port, it.Name, hint))
			}
		}
	}
	return errors.Join(errs...)
}

func missingSecrets(r *Recipe, have map[string]string) []string {
	var out []string
	for _, s := range r.Secrets() {
		if _, ok := have[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

func (in *Installer) run(ctx context.Context, j *job, r *Recipe, profile string, req ApplyRequest) {
	defer close(j.done)
	defer j.cancel()
	err := in.install(ctx, j, r, profile, req)
	st := j.status()
	state, msg := StateInstalled, ""
	switch {
	case err != nil && ctx.Err() != nil:
		state, msg = StateCanceled, "canceled"
	case err != nil:
		// A failed setup step's output goes to installed.json: never with a
		// secret in it.
		state, msg = StateFailed, hideSecrets(err.Error(), req.Secrets)
	}
	if err != nil {
		j.logf("%s: %s", state, msg)
	} else {
		j.logf("installed %s %s (profile %s)", r.Name, r.Version, profileName(profile))
	}
	j.set(func(s *Status) {
		s.State, s.Error, s.FinishedAt, s.Current = state, msg, time.Now().UTC(), ""
		s.SpeedBps, s.ETASec = 0, 0
	})
	in.Store.Update(r.Name, func(it *Installed) {
		it.State, it.Phase, it.Error = state, st.Phase, msg
	})
}

func profileName(p string) string {
	if p == "" {
		return "all"
	}
	return p
}

func (in *Installer) phase(j *job, r *Recipe, phase string) {
	j.set(func(s *Status) { s.Phase, s.Current = phase, "" })
	in.Store.Update(r.Name, func(it *Installed) { it.Phase = phase })
}

func (in *Installer) install(ctx context.Context, j *job, r *Recipe, profile string, req ApplyRequest) error {
	if err := in.Env.Check(ctx, r.Requires); err != nil {
		return fmt.Errorf("this machine cannot have %s: %w", r.Name, err)
	}
	x, err := r.Expand(Vars{Data: in.Data, Secrets: req.Secrets})
	if err != nil {
		return err
	}
	root := x.Root
	if !filepath.IsAbs(root) {
		return fmt.Errorf("root %q is not an absolute path on this machine", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}

	j.logf("planning %s %s in %s", r.Name, r.Version, root)
	plan, err := MakePlan(x, os.DirFS(root), PlanOptions{Profile: profile, Verify: req.Verify, Ctx: ctx})
	if err != nil {
		return err
	}
	j.set(func(s *Status) { s.Plan, s.Total = plan, plan.FetchBytes })
	for _, w := range plan.Warnings {
		j.logf("warning: %s", w)
	}
	for _, a := range plan.Artifacts {
		if a.State == Conflict {
			return fmt.Errorf("%s: %s is in the way and is not a file; remove it by hand", a.ID, a.Path)
		}
	}
	if plan.FetchBytes > 0 {
		free, err := in.Env.FreeBytes(root)
		if err == nil && free < plan.FetchBytes {
			return fmt.Errorf("not enough disk space in %s: need %s, free %s", root, human(plan.FetchBytes), human(free))
		}
	}
	arts := map[string]Artifact{}
	for _, a := range x.Artifacts {
		arts[a.ID] = a
	}
	abs := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

	in.phase(j, r, PhaseFetch)
	for _, ap := range plan.Artifacts {
		if !ap.Fetch {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		a := arts[ap.ID]
		dest := abs(a.Path)
		j.set(func(s *Status) { s.Current = a.ID })
		if ap.State == WrongSize || ap.State == WrongHash {
			j.logf("%s: %s on disk (%s), downloading it again", a.ID, ap.State, a.Path)
			if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("%s: %w", a.ID, err)
			}
		}
		if ap.State == Partial {
			j.logf("%s: resuming at %s of %s", a.ID, human(ap.Have), human(a.Size))
		} else {
			j.logf("%s: downloading %s", a.ID, human(a.Size))
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		start, got := ap.Have, int64(0)
		from, err := in.Env.Fetch(ctx, a, req.Sources, dest, func(n int64) {
			// A source that restarts from 0 (its .part was dropped) counts
			// again from the resume point; nothing goes negative.
			got = max(0, n-start)
			j.progress(got)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", a.ID, err)
		}
		if a.Size > 0 {
			got = a.Size - start
		}
		j.mu.Lock()
		j.fetched += got
		j.mu.Unlock()
		j.progress(0)
		j.logf("%s: done, from %s", a.ID, from)
	}

	in.phase(j, r, PhaseUnpack)
	for _, ap := range plan.Artifacts {
		if !ap.Unpack {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		a := arts[ap.ID]
		j.set(func(s *Status) { s.Current = a.ID })
		to := abs(a.Unpack.To)
		j.logf("%s: unpacking into %s", a.ID, to)
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		// The marker stays if the unpack dies half way, so the next plan does
		// not take unpack.creates as proof and unpacks again.
		marker := abs(UnpackMarker(a.ID))
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			return fmt.Errorf("%s: %w", a.ID, err)
		}
		if err := in.Env.Unpack(ctx, abs(a.Path), to); err != nil {
			return fmt.Errorf("%s: unpack: %w", a.ID, err)
		}
		if _, err := os.Stat(abs(a.Unpack.Creates)); err != nil {
			return fmt.Errorf("%s: unpacked, but %s is not there: unpack.creates or unpack.to is wrong", a.ID, a.Unpack.Creates)
		}
		if err := os.Remove(marker); err != nil {
			return fmt.Errorf("%s: %w", a.ID, err)
		}
	}

	in.phase(j, r, PhaseFiles)
	for i, fp := range plan.Files {
		if fp.State == FileSame {
			continue
		}
		f := x.Files[i]
		dest := abs(f.Path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if strings.Contains(r.Files[i].Content, "${secret:") {
			perm = 0o600
		}
		if fp.State == FileDiffers {
			// Keep the first version we replace, in case it was edited by hand.
			bak := dest + ".astrea-bak"
			if _, err := os.Stat(bak); os.IsNotExist(err) {
				if old, err := os.ReadFile(dest); err == nil {
					os.WriteFile(bak, old, perm)
				}
			}
		}
		if err := writeFileAtomic(dest, []byte(f.Content), perm); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		j.logf("%s: %s, written", f.Path, fp.State)
	}

	in.phase(j, r, PhaseSetup)
	for _, s := range x.Setup {
		if err := ctx.Err(); err != nil {
			return err
		}
		j.set(func(st *Status) { st.Current = s.Name })
		if s.Creates != "" {
			if _, err := os.Stat(s.Creates); err == nil {
				j.logf("setup %q: already done (%s exists)", s.Name, s.Creates)
				continue
			}
		}
		if s.Unless != "" {
			out, err := in.Env.Run(ctx, s.Shell, s.Unless)
			if err == nil && strings.TrimSpace(out) != "" {
				j.logf("setup %q: already done (unless succeeded)", s.Name)
				continue
			}
		}
		j.logf("setup %q: running", s.Name)
		if out, err := in.Env.Run(ctx, s.Shell, s.Run); err != nil {
			if out = strings.TrimSpace(out); out != "" {
				return fmt.Errorf("setup %q: %w; output: %s", s.Name, err, clip(out, 500))
			}
			return fmt.Errorf("setup %q: %w", s.Name, err)
		}
	}

	in.phase(j, r, PhaseServices)
	for _, s := range x.Services {
		if err := ctx.Err(); err != nil {
			return err
		}
		j.set(func(st *Status) { st.Current = s.Name })
		note, err := in.Env.StartService(ctx, r.Name, s)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		j.logf("service %q: %s", s.Name, note)
	}
	return nil
}

// Status returns the running or last install of name.
func (in *Installer) Status(name string) (Status, error) {
	in.mu.Lock()
	j := in.jobs[name]
	in.mu.Unlock()
	if j != nil {
		return j.status(), nil
	}
	it, ok := in.Store.Get(name)
	if !ok {
		return Status{}, fmt.Errorf("recipe %q is not installed here", name)
	}
	st := Status{Name: it.Name, Version: it.Version, Profile: it.Profile, State: it.State, Phase: it.Phase, Error: it.Error, StartedAt: it.StartedAt}
	if it.State == StateInstalling {
		// Recorded as installing but no job: the agent restarted mid-install.
		st.State = StateFailed
		st.Error = "interrupted (the agent restarted); apply it again to resume"
	}
	return st, nil
}

// Wait blocks until the install of name ends or ctx is done.
func (in *Installer) Wait(ctx context.Context, name string) (Status, error) {
	in.mu.Lock()
	j := in.jobs[name]
	in.mu.Unlock()
	if j != nil {
		select {
		case <-j.done:
		case <-ctx.Done():
		}
	}
	return in.Status(name)
}

// Cancel stops a running install. Finished downloads stay; the one in
// progress keeps its .part file, so applying again resumes it.
func (in *Installer) Cancel(name string) (Status, error) {
	in.mu.Lock()
	j := in.jobs[name]
	in.mu.Unlock()
	if j == nil {
		return in.Status(name)
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(cancelWait):
		// A setup command that ignores the kill, say: report it rather than
		// hang the caller. The job still ends as canceled.
		st := j.status()
		st.Error = "cancel requested; still stopping"
		return st, nil
	}
	return j.status(), nil
}

// cancelWait bounds how long Cancel waits for the install to stop.
var cancelWait = 30 * time.Second

// Remove stops and forgets the recipe's services and endpoints and drops it
// from installed.json. With purge it also deletes what the recipe put on
// disk: its artifacts, its files and the folders it unpacked into (never the
// root itself, which may hold other things).
func (in *Installer) Remove(ctx context.Context, name string, purge bool) ([]string, error) {
	in.mu.Lock()
	if in.jobs == nil {
		in.jobs = map[string]*job{}
	}
	if j := in.jobs[name]; j != nil {
		select {
		case <-j.done:
		default:
			in.mu.Unlock()
			if j.removing {
				return nil, fmt.Errorf("%s is already being removed", name)
			}
			return nil, fmt.Errorf("%s is being installed; cancel it first", name)
		}
	}
	// A placeholder job, not done until Remove returns: Apply refuses to
	// start while files are being deleted.
	hold := &job{cancel: func() {}, done: make(chan struct{}), removing: true}
	hold.st = Status{Name: name, State: "removing"}
	in.jobs[name] = hold
	in.mu.Unlock()
	defer func() {
		in.mu.Lock()
		if in.jobs[name] == hold {
			delete(in.jobs, name)
		}
		in.mu.Unlock()
		close(hold.done)
	}()

	it, ok := in.Store.Get(name)
	if !ok {
		return nil, fmt.Errorf("recipe %q is not installed here", name)
	}
	r, err := Parse(it.Recipe)
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for _, s := range r.Services {
		if err := in.Env.StopService(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("service %q: %w", s.Name, err))
		} else {
			done = append(done, "service "+s.Name+" removed")
		}
	}
	if purge {
		root, err := r.RootFor(in.Data)
		if err != nil {
			return done, err
		}
		abs := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
		for _, a := range r.Artifacts {
			for _, p := range []string{abs(a.Path), abs(a.Path) + ".part"} {
				if err := os.Remove(p); err == nil {
					done = append(done, "deleted "+p)
				} else if !os.IsNotExist(err) {
					errs = append(errs, err)
				}
			}
			if a.Unpack != nil && a.Unpack.To != "" {
				if err := os.RemoveAll(abs(a.Unpack.To)); err != nil {
					errs = append(errs, err)
				} else {
					done = append(done, "deleted "+abs(a.Unpack.To))
				}
			}
		}
		for _, f := range r.Files {
			if err := os.Remove(abs(f.Path)); err == nil {
				done = append(done, "deleted "+abs(f.Path))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return done, err
	}
	return done, in.Store.Delete(name)
}

// Resume restarts the installs that were running when the agent stopped,
// except those that need secrets (they are not stored). Call once at start.
func (in *Installer) Resume() []string {
	var resumed []string
	for _, it := range in.Store.List() {
		if it.State != StateInstalling {
			continue
		}
		if it.NeedsSecrets {
			in.Store.Update(it.Name, func(x *Installed) {
				x.State, x.Error = StateFailed, "interrupted; it needs secrets, so apply it again from the panel"
			})
			continue
		}
		if _, err := in.Apply(ApplyRequest{Recipe: it.Recipe, Profile: it.Profile}); err != nil {
			in.Store.Update(it.Name, func(x *Installed) { x.State, x.Error = StateFailed, "resume: "+err.Error() })
			continue
		}
		resumed = append(resumed, it.Name)
	}
	return resumed
}

// hideSecrets replaces the values of secrets in s with ***.
func hideSecrets(s string, secrets map[string]string) string {
	for _, v := range secrets {
		if len(v) >= 4 { // shorter ones would mangle ordinary words
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".astrea-tmp"
	os.Remove(tmp) // WriteFile keeps the mode of a leftover one
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

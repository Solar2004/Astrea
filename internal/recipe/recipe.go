// Package recipe defines Astrea service recipes: a small JSON document that
// describes everything needed to install and run a service on a machine
// (large artifacts with their mirrors, small config files, idempotent setup
// steps, supervised services and the endpoints they publish).
//
// The package does not depend on the agent, so the panel can use the same
// Validate and plan to show what an install will do before doing it, and it
// can be tested on any OS. See docs/SERVICES.md.
package recipe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Schema is the only recipe format this package understands.
const Schema = "astrea.recipe/v1"

// Recipe is one service recipe.
type Recipe struct {
	Schema      string             `json:"schema"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
	Requires    Requires           `json:"requires,omitzero"`
	Root        string             `json:"root"`
	Profiles    map[string]Profile `json:"profiles,omitempty"`
	Artifacts   []Artifact         `json:"artifacts,omitempty"`
	Files       []File             `json:"files,omitempty"`
	Setup       []Step             `json:"setup,omitempty"`
	Services    []Service          `json:"services,omitempty"`
	Resources   Resources          `json:"resources,omitzero"`
	// Global recipes are applied by the panel to every machine that meets
	// Requires, including machines that join later.
	Global bool     `json:"global,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// Requires says which machines can have the recipe. Empty fields match any.
type Requires struct {
	OS     string `json:"os,omitempty"`  // windows, linux or darwin
	GPU    string `json:"gpu,omitempty"` // nvidia
	VRAMMB int    `json:"vram_mb,omitempty"`
}

// Profile is a named subset of artifact groups ("basic", "full"…).
type Profile struct {
	Description string   `json:"description,omitempty"`
	Groups      []string `json:"groups"`
	Default     bool     `json:"default,omitempty"`
}

// Artifact is a large file, identified by its path under Root, size and hash.
type Artifact struct {
	ID string `json:"id"`
	// Group ties the artifact to profiles. Artifacts without a group belong to
	// every profile.
	Group  string   `json:"group,omitempty"`
	Path   string   `json:"path"` // relative to Root, with '/' separators
	Size   int64    `json:"size,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	URLs   []string `json:"urls,omitempty"` // http(s):// or hf://<repo>/<file>
	Unpack *Unpack  `json:"unpack,omitempty"`
	// Pack names a folder under Root that recipe.capture zips on the source
	// machine to produce this artifact (an embedded Python, custom nodes…).
	// Its size and hash are only known once it has been packed.
	Pack string `json:"pack,omitempty"`
	Note string `json:"note,omitempty"`
}

// Unpack extracts an archive artifact once it is complete.
type Unpack struct {
	To string `json:"to"` // relative to Root; "" is Root itself
	// Creates is a path under Root that exists once the archive has been
	// unpacked. While it exists the archive itself is not needed, so a
	// machine that already has the unpacked folder downloads nothing.
	Creates string `json:"creates"`
}

// File is a small file written verbatim (configs).
type File struct {
	Path    string `json:"path"` // relative to Root, with '/' separators
	Content string `json:"content"`
}

// Step is an idempotent setup command.
type Step struct {
	Name  string `json:"name"`
	Shell string `json:"shell,omitempty"`
	Run   string `json:"run"`
	// Unless is a command; if it succeeds and prints something, the step is
	// already done. Creates is a path; if it exists, the step is already done.
	Unless  string `json:"unless,omitempty"`
	Creates string `json:"creates,omitempty"`
}

// Service is a supervised long-lived process (what service.start runs today).
type Service struct {
	Name      string            `json:"name"`
	Command   string            `json:"command"`
	Shell     string            `json:"shell,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Restart   string            `json:"restart,omitempty"`   // on-failure (default), always, never
	Autostart *bool             `json:"autostart,omitempty"` // default true, false if OnDemand
	OnDemand  *OnDemand         `json:"on_demand,omitempty"`
	Ready     *Ready            `json:"ready,omitempty"`
	Expose    *Expose           `json:"expose,omitempty"`
}

// OnDemand services are not started with the agent: something starts them
// when needed and they stop after IdleStopSec without use.
type OnDemand struct {
	IdleStopSec int `json:"idle_stop_sec"`
}

// Ready is the readiness probe: a URL that must return 200, or a local port.
type Ready struct {
	URL  string `json:"url,omitempty"`
	Port int    `json:"port,omitempty"`
}

// Expose publishes 127.0.0.1:Port as /dyn/<Endpoint> on the agent.
type Expose struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
	API      string `json:"api"`            // openai, comfyui or http
	Auth     string `json:"auth"`           // none (tailnet only) or key
	Docs     string `json:"docs,omitempty"` // for api http: where its API is described
}

// Resources is what the recipe's services need from the machine while running.
type Resources struct {
	GPUMB        int  `json:"gpu_mb,omitempty"`
	GPUExclusive bool `json:"gpu_exclusive,omitempty"`
}

// Parse decodes a recipe. Unknown fields are an error, so typos in a
// hand- or AI-written recipe don't silently drop a setting. It does not
// call Validate.
func Parse(data []byte) (*Recipe, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Recipe
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	if dec.More() {
		return nil, errors.New("recipe: trailing data after the JSON object")
	}
	return &r, nil
}

// Load reads, parses and validates a recipe file.
func Load(file string) (*Recipe, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	r, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return r, nil
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	versionRe = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	varRe     = regexp.MustCompile(`\$\{([^}]*)\}`)
	shells    = []string{"default", "cmd", "powershell", "pwsh", "sh", "bash", "python", "node"}
)

// Validate checks the whole recipe and reports every problem it finds, not
// just the first one.
func (r *Recipe) Validate() error {
	v := &validator{}
	if r.Schema != Schema {
		v.addf("schema: want %q, got %q", Schema, r.Schema)
	}
	v.name("name", r.Name)
	if !versionRe.MatchString(r.Version) {
		v.addf("version: %q is not MAJOR.MINOR.PATCH", r.Version)
	}
	switch r.Requires.OS {
	case "", "windows", "linux", "darwin":
	default:
		v.addf("requires.os: unknown %q (windows, linux or darwin)", r.Requires.OS)
	}
	switch r.Requires.GPU {
	case "", "nvidia":
	default:
		v.addf("requires.gpu: unknown %q (nvidia)", r.Requires.GPU)
	}
	if r.Requires.VRAMMB < 0 {
		v.addf("requires.vram_mb: negative")
	}
	if strings.TrimSpace(r.Root) == "" {
		v.addf("root: required")
	} else if strings.Contains(r.Root, "${root}") {
		v.addf("root: cannot refer to ${root}")
	} else if !absRoot(r.Root) {
		// A relative root would resolve against the agent's working
		// directory (System32 for the service): installs and purges there.
		v.addf("root: %q must be absolute (C:\\x or /x) or start with ${data}", r.Root)
	}
	v.noSecrets("root", r.Root)

	groups := map[string]bool{}
	ids := map[string]bool{}
	paths := map[string]string{}
	for i, a := range r.Artifacts {
		at := fmt.Sprintf("artifacts[%d]", i)
		if a.ID != "" {
			at = fmt.Sprintf("artifact %q", a.ID)
		}
		v.name(at+".id", a.ID)
		if ids[a.ID] {
			v.addf("%s: duplicate id", at)
		}
		ids[a.ID] = true
		if a.Group != "" {
			v.name(at+".group", a.Group)
			groups[a.Group] = true
		}
		v.relPath(at+".path", a.Path, false)
		if other, dup := paths[a.Path]; dup {
			v.addf("%s: path %q is also used by %s", at, a.Path, other)
		}
		paths[a.Path] = at
		if a.Size < 0 {
			v.addf("%s.size: negative", at)
		}
		if a.Pack == "" && a.Size == 0 {
			v.addf("%s.size: required unless the artifact is a pack", at)
		}
		if a.SHA256 != "" && !sha256Re.MatchString(a.SHA256) {
			v.addf("%s.sha256: want 64 lowercase hex digits", at)
		}
		for j, u := range a.URLs {
			v.url(fmt.Sprintf("%s.urls[%d]", at, j), u)
		}
		if a.Pack != "" {
			v.relPath(at+".pack", a.Pack, false)
		}
		if a.Unpack != nil {
			v.relPath(at+".unpack.to", a.Unpack.To, true)
			v.relPath(at+".unpack.creates", a.Unpack.Creates, false)
		}
	}

	defaults := 0
	for _, name := range sortedKeys(r.Profiles) {
		p := r.Profiles[name]
		at := fmt.Sprintf("profile %q", name)
		v.name(at, name)
		if len(p.Groups) == 0 {
			v.addf("%s: no groups", at)
		}
		for _, g := range p.Groups {
			if !groups[g] {
				v.addf("%s: no artifact has group %q", at, g)
			}
		}
		if p.Default {
			defaults++
		}
	}
	if len(r.Profiles) > 1 && defaults != 1 {
		v.addf("profiles: exactly one must be the default, found %d", defaults)
	}
	if len(r.Profiles) == 0 && len(groups) > 0 {
		v.addf("profiles: artifacts have groups but there are no profiles to pick them")
	}

	for i, f := range r.Files {
		at := fmt.Sprintf("files[%d]", i)
		v.relPath(at+".path", f.Path, false)
		if other, dup := paths[f.Path]; dup {
			v.addf("%s: path %q is also used by %s", at, f.Path, other)
		}
		paths[f.Path] = at
		v.secrets(at+".content", f.Content)
	}

	steps := map[string]bool{}
	for i, s := range r.Setup {
		at := fmt.Sprintf("setup[%d]", i)
		if s.Name == "" {
			v.addf("%s.name: required", at)
		} else {
			at = fmt.Sprintf("setup %q", s.Name)
		}
		if steps[s.Name] {
			v.addf("%s: duplicate name", at)
		}
		steps[s.Name] = true
		v.shell(at+".shell", s.Shell)
		if strings.TrimSpace(s.Run) == "" {
			v.addf("%s.run: required", at)
		}
		// Steps run again on every apply: without a check, one that is not
		// idempotent breaks reinstalling (docs/SERVICES.md §7.7).
		if s.Unless == "" && s.Creates == "" {
			v.addf("%s: needs unless or creates to know when it is already done", at)
		}
		v.secrets(at+".run", s.Run)
		v.secrets(at+".unless", s.Unless)
		v.secrets(at+".creates", s.Creates)
	}

	svcs := map[string]bool{}
	endpoints := map[string]bool{}
	ports := map[int]string{}
	for i, s := range r.Services {
		at := fmt.Sprintf("services[%d]", i)
		if s.Name != "" {
			at = fmt.Sprintf("service %q", s.Name)
		}
		v.name(at+".name", s.Name)
		if svcs[s.Name] {
			v.addf("%s: duplicate name", at)
		}
		svcs[s.Name] = true
		if strings.TrimSpace(s.Command) == "" {
			v.addf("%s.command: required", at)
		}
		v.shell(at+".shell", s.Shell)
		switch s.Restart {
		case "", "on-failure", "always", "never":
		default:
			v.addf("%s.restart: unknown %q (on-failure, always or never)", at, s.Restart)
		}
		if s.OnDemand != nil {
			if s.OnDemand.IdleStopSec <= 0 {
				v.addf("%s.on_demand.idle_stop_sec: must be positive", at)
			}
			if s.Autostart != nil && *s.Autostart {
				v.addf("%s: on_demand services cannot autostart", at)
			}
		}
		if rd := s.Ready; rd != nil {
			if (rd.URL == "") == (rd.Port == 0) {
				v.addf("%s.ready: set exactly one of url or port", at)
			}
			if rd.URL != "" {
				if u, err := url.Parse(rd.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					v.addf("%s.ready.url: want an http(s) url, got %q", at, rd.URL)
				}
			}
			if rd.Port != 0 {
				v.port(at+".ready.port", rd.Port)
			}
		}
		if e := s.Expose; e != nil {
			v.name(at+".expose.endpoint", e.Endpoint)
			if endpoints[e.Endpoint] {
				v.addf("%s.expose.endpoint: %q is already exposed by another service", at, e.Endpoint)
			}
			endpoints[e.Endpoint] = true
			v.port(at+".expose.port", e.Port)
			if other, dup := ports[e.Port]; dup {
				v.addf("%s.expose.port: %d is also used by %s", at, e.Port, other)
			}
			ports[e.Port] = at
			switch e.API {
			case "openai", "comfyui", "http":
			default:
				v.addf("%s.expose.api: unknown %q (openai, comfyui or http)", at, e.API)
			}
			switch e.Auth {
			case "none", "key":
			default:
				v.addf("%s.expose.auth: want none or key, got %q", at, e.Auth)
			}
		}
		// The supervisor stores service definitions in services.json, so a
		// secret here would end up on disk in clear.
		v.noSecrets(at+".command", s.Command)
		v.noSecrets(at+".cwd", s.Cwd)
		for _, k := range sortedKeys(s.Env) {
			v.noSecrets(at+".env."+k, s.Env[k])
		}
	}

	if r.Resources.GPUMB < 0 {
		v.addf("resources.gpu_mb: negative")
	}
	if r.Resources.GPUExclusive && r.Resources.GPUMB == 0 {
		v.addf("resources: gpu_exclusive without gpu_mb")
	}
	return v.err()
}

// AutostartOf reports whether the service starts with the agent.
func (s Service) AutostartOf() bool {
	if s.Autostart != nil {
		return *s.Autostart
	}
	return s.OnDemand == nil
}

// DefaultProfile returns the profile used when none is chosen ("" if the
// recipe has no profiles: then every artifact is installed).
func (r *Recipe) DefaultProfile() string {
	for _, name := range sortedKeys(r.Profiles) {
		if r.Profiles[name].Default {
			return name
		}
	}
	if len(r.Profiles) == 1 {
		for name := range r.Profiles {
			return name
		}
	}
	return ""
}

// ProfileArtifacts returns the artifacts a profile installs, in recipe order.
// An empty profile means the default one.
func (r *Recipe) ProfileArtifacts(profile string) ([]Artifact, error) {
	if profile == "" {
		profile = r.DefaultProfile()
	}
	if profile == "" {
		if len(r.Profiles) > 0 {
			return nil, errors.New("recipe has profiles but none is the default: choose one")
		}
		return slices.Clone(r.Artifacts), nil
	}
	p, ok := r.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q (have %s)", profile, strings.Join(sortedKeys(r.Profiles), ", "))
	}
	var out []Artifact
	for _, a := range r.Artifacts {
		if a.Group == "" || slices.Contains(p.Groups, a.Group) {
			out = append(out, a)
		}
	}
	return out, nil
}

// Size is the total known size of the artifacts in a profile. Packs whose
// size is not known yet count as 0.
func (r *Recipe) Size(profile string) (int64, error) {
	arts, err := r.ProfileArtifacts(profile)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, a := range arts {
		n += a.Size
	}
	return n, nil
}

// Secrets lists the ${secret:name} references in the recipe, sorted and
// without duplicates, so the panel knows what to ask for before installing.
func (r *Recipe) Secrets() []string {
	var out []string
	r.walkStrings(func(s *string) {
		for _, m := range varRe.FindAllStringSubmatch(*s, -1) {
			if name, ok := strings.CutPrefix(m[1], "secret:"); ok && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	})
	slices.Sort(out)
	return out
}

// Vars are the values substituted by Expand.
type Vars struct {
	// Data is the agent's data directory, ${data}.
	Data    string
	Secrets map[string]string
}

// Expand returns a copy of the recipe with ${root}, ${data} and
// ${secret:name} replaced in the root, file contents, setup steps and
// services. Other ${…} are left untouched: PowerShell uses ${env:PATH} and
// shell scripts their own variables. Artifact paths and URLs are not
// expanded. A referenced secret or ${data} without a value is an error.
func (r *Recipe) Expand(vars Vars) (*Recipe, error) {
	out := r.clone()
	var errs []error
	expand := func(s string, root string) string {
		return varRe.ReplaceAllStringFunc(s, func(m string) string {
			name := m[2 : len(m)-1]
			switch {
			case name == "root" && root != "":
				return root
			case name == "data":
				if vars.Data == "" {
					errs = append(errs, errors.New("${data} is used but has no value"))
				}
				return vars.Data
			case strings.HasPrefix(name, "secret:"):
				key := strings.TrimPrefix(name, "secret:")
				val, ok := vars.Secrets[key]
				if !ok {
					errs = append(errs, fmt.Errorf("secret %q is used but has no value", key))
				}
				return val
			}
			return m
		})
	}
	out.Root = expand(out.Root, "")
	out.walkExpandable(func(s *string) { *s = expand(*s, out.Root) })
	if err := errors.Join(dedupe(errs)...); err != nil {
		return nil, err
	}
	return out, nil
}

// walkExpandable visits the strings Expand substitutes in, except Root.
func (r *Recipe) walkExpandable(fn func(*string)) {
	for i := range r.Files {
		fn(&r.Files[i].Content)
	}
	for i := range r.Setup {
		s := &r.Setup[i]
		fn(&s.Run)
		fn(&s.Unless)
		fn(&s.Creates)
	}
	for i := range r.Services {
		s := &r.Services[i]
		fn(&s.Command)
		fn(&s.Cwd)
		for k, val := range s.Env {
			fn(&val)
			s.Env[k] = val
		}
		if s.Ready != nil {
			fn(&s.Ready.URL)
		}
	}
}

// walkStrings visits Root and every expandable string.
func (r *Recipe) walkStrings(fn func(*string)) {
	fn(&r.Root)
	r.walkExpandable(fn)
}

// clone deep-copies the parts of the recipe that Expand modifies.
func (r *Recipe) clone() *Recipe {
	out := *r
	out.Files = slices.Clone(r.Files)
	out.Setup = slices.Clone(r.Setup)
	out.Services = slices.Clone(r.Services)
	for i := range out.Services {
		s := &out.Services[i]
		if s.Env != nil {
			env := make(map[string]string, len(s.Env))
			for k, val := range s.Env {
				env[k] = val
			}
			s.Env = env
		}
		if s.Ready != nil {
			rd := *s.Ready
			s.Ready = &rd
		}
	}
	return &out
}

type validator struct{ errs []error }

func (v *validator) addf(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

func (v *validator) err() error {
	if len(v.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid recipe: %w", errors.Join(v.errs...))
}

func (v *validator) name(at, s string) {
	if !nameRe.MatchString(s) {
		v.addf("%s: %q is not a valid name (1-64 letters, digits, '.', '_' or '-')", at, s)
	}
}

func (v *validator) port(at string, p int) {
	if p <= 0 || p > 65535 {
		v.addf("%s: %d is not a port (1-65535)", at, p)
	}
}

func (v *validator) shell(at, s string) {
	if s != "" && !slices.Contains(shells, s) {
		v.addf("%s: unknown %q (%s)", at, s, strings.Join(shells, ", "))
	}
}

// relPath checks a path that is joined to Root on any OS: forward slashes,
// relative, clean and not escaping Root.
func (v *validator) relPath(at, p string, emptyOK bool) {
	switch {
	case p == "":
		if !emptyOK {
			v.addf("%s: required", at)
		}
	case strings.Contains(p, `\`):
		v.addf("%s: %q must use '/' separators", at, p)
	case strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':'):
		v.addf("%s: %q must be relative to root", at, p)
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../"):
		v.addf("%s: %q is not a clean path inside root", at, p)
	case strings.ContainsAny(p, `:*?"<>|`):
		// ':' elsewhere is an NTFS stream (a.bin:x), the rest are invalid there.
		v.addf("%s: %q has a character Windows does not allow in file names", at, p)
	default:
		for _, seg := range strings.Split(p, "/") {
			if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") || reservedName(seg) {
				v.addf("%s: %q: %q is not a valid file name on Windows", at, p, seg)
				break
			}
		}
	}
}

// absRoot reports whether root is absolute on Windows or Unix once ${data}
// (always absolute) is expanded.
func absRoot(root string) bool {
	if strings.HasPrefix(root, "${data}") {
		return true
	}
	if strings.HasPrefix(root, "/") {
		return true
	}
	return len(root) >= 3 && root[1] == ':' && (root[2] == '\\' || root[2] == '/') &&
		(root[0]|0x20 >= 'a' && root[0]|0x20 <= 'z')
}

// reservedName reports DOS device names (CON, NUL, COM1…), which Windows
// treats as devices whatever the extension: "nul.txt" is NUL.
func reservedName(seg string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(seg), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func (v *validator) url(at, s string) {
	u, err := url.Parse(s)
	if err != nil {
		v.addf("%s: %v", at, err)
		return
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			v.addf("%s: %q has no host", at, s)
		}
	case "hf":
		// hf://<owner>/<repo>/<file…>
		if u.Host == "" || strings.Count(strings.Trim(u.Path, "/"), "/") < 1 {
			v.addf("%s: %q: want hf://<owner>/<repo>/<file>", at, s)
		}
	default:
		v.addf("%s: unsupported scheme in %q (http, https or hf)", at, s)
	}
}

// secrets checks that every ${secret:…} has a usable name.
func (v *validator) secrets(at, s string) {
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		if name, ok := strings.CutPrefix(m[1], "secret:"); ok && !nameRe.MatchString(name) {
			v.addf("%s: bad secret name in %q", at, m[0])
		}
	}
}

func (v *validator) noSecrets(at, s string) {
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		if strings.HasPrefix(m[1], "secret:") {
			v.addf("%s: %s: services cannot use secrets yet (their definition is stored on disk); read it from a file written by files[] instead", at, m[0])
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func dedupe(errs []error) []error {
	var out []error
	seen := map[string]bool{}
	for _, e := range errs {
		if !seen[e.Error()] {
			seen[e.Error()] = true
			out = append(out, e)
		}
	}
	return out
}

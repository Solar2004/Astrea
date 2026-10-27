package recipe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Artifact states in a plan.
const (
	// Present: the file is there with the right size (and hash, if checked).
	Present = "present"
	// Unpacked: the archive's unpack.creates exists, so the archive itself is
	// not needed.
	Unpacked = "unpacked"
	// Missing: nothing on disk.
	Missing = "missing"
	// Partial: an interrupted download left <path>.part; it resumes from Have.
	Partial = "partial"
	// WrongSize / WrongHash: a file is there but it is not this artifact. It
	// is downloaded again.
	WrongSize = "wrong_size"
	WrongHash = "wrong_hash"
	// Conflict: something that is not a regular file is in the way. The plan
	// cannot be applied until it is removed by hand.
	Conflict = "conflict"
)

// File states in a plan.
const (
	FileSame    = "same"
	FileMissing = "missing"
	FileDiffers = "differs"
)

// Plan is what applying a recipe to one machine will do, computed by
// comparing the recipe with what is on disk.
type Plan struct {
	Recipe    string         `json:"recipe"`
	Version   string         `json:"version"`
	Profile   string         `json:"profile,omitempty"`
	Artifacts []ArtifactPlan `json:"artifacts"`
	// Extra are artifacts of the recipe that are on disk but not in the
	// chosen profile. They are left alone; removing them is a separate
	// "clean" step (docs/SERVICES.md §7.12).
	Extra    []ArtifactPlan `json:"extra,omitempty"`
	Files    []FilePlan     `json:"files,omitempty"`
	Setup    []string       `json:"setup,omitempty"`
	Services []string       `json:"services,omitempty"`
	// TotalBytes is the known size of the profile; FetchBytes what is still
	// to download (resumed .part files count only their missing tail).
	TotalBytes int64    `json:"total_bytes"`
	FetchBytes int64    `json:"fetch_bytes"`
	Warnings   []string `json:"warnings,omitempty"`
}

// ArtifactPlan is the state of one artifact and what will be done with it.
type ArtifactPlan struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	State    string `json:"state"`
	Size     int64  `json:"size,omitempty"` // expected; 0 for a pack not built yet
	Have     int64  `json:"have,omitempty"` // bytes already usable (file or .part)
	Verified bool   `json:"verified,omitempty"`
	Fetch    bool   `json:"fetch,omitempty"`
	Unpack   bool   `json:"unpack,omitempty"`
	// Mirrors is how many internet sources the recipe lists. With none, the
	// artifact can only be copied from another machine that has it.
	Mirrors int `json:"mirrors"`
}

// FilePlan is the state of one small file.
type FilePlan struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

// PlanOptions tunes MakePlan.
type PlanOptions struct {
	// Profile to install; "" is the recipe's default.
	Profile string
	// Verify hashes artifacts that have the right size and a sha256 in the
	// recipe. Without it, size alone decides (fast, used to preview).
	Verify bool
	// Hash returns the hex sha256 of a file in fsys. Nil hashes it in full;
	// the agent passes a cached one (docs/SERVICES.md §7.4).
	Hash func(fsys fs.FS, name string) (string, error)
	// Ctx, if set, stops the default hash and the plan when done: verifying
	// 100 GB takes minutes and an install must stay cancelable.
	Ctx context.Context
}

// UnpackMarker is the file under root that says an archive's unpack started
// and did not finish. While it is there, unpack.creates proves nothing (a
// killed tar may have written it first) and the archive is unpacked again.
func UnpackMarker(id string) string { return ".astrea-unpacking-" + id }

// MakePlan compares an expanded recipe (see Expand) with the files under its
// root, given as fsys (os.DirFS(root) on the agent). It does not change
// anything. Setup steps and services are always listed: whether a step is
// already done is decided when it runs (unless/creates).
func MakePlan(r *Recipe, fsys fs.FS, opt PlanOptions) (*Plan, error) {
	if s := r.Secrets(); len(s) > 0 {
		return nil, fmt.Errorf("recipe still has ${secret:…} references (%s): call Expand first", strings.Join(s, ", "))
	}
	profile := opt.Profile
	if profile == "" {
		profile = r.DefaultProfile()
	}
	arts, err := r.ProfileArtifacts(profile)
	if err != nil {
		return nil, err
	}
	ctx := opt.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	hash := opt.Hash
	if hash == nil {
		hash = func(fsys fs.FS, name string) (string, error) { return hashFile(ctx, fsys, name) }
	}

	p := &Plan{Recipe: r.Name, Version: r.Version, Profile: profile}
	inProfile := map[string]bool{}
	for _, a := range arts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inProfile[a.ID] = true
		ap, err := planArtifact(fsys, a, opt.Verify, hash)
		if err != nil {
			return nil, err
		}
		p.TotalBytes += a.Size
		if ap.Fetch {
			p.FetchBytes += a.Size - ap.Have
		}
		switch {
		case ap.State == Conflict:
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %s is in the way and is not a file; remove it by hand", a.ID, a.Path))
		case ap.Fetch && ap.Mirrors == 0:
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: no mirror, it can only be copied from another machine that has it", a.ID))
		}
		if a.Pack != "" && a.Size == 0 && ap.Fetch {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: pack not built yet (run recipe.capture on a machine that has %s)", a.ID, a.Pack))
		}
		p.Artifacts = append(p.Artifacts, ap)
	}
	for _, a := range r.Artifacts {
		if inProfile[a.ID] {
			continue
		}
		ap, err := planArtifact(fsys, a, false, hash)
		if err != nil {
			return nil, err
		}
		if ap.State == Present || ap.State == Unpacked || ap.State == Partial {
			p.Extra = append(p.Extra, ap)
		}
	}

	for _, f := range r.Files {
		fp := FilePlan{Path: f.Path, State: FileSame}
		data, err := fs.ReadFile(fsys, f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fp.State = FileMissing
		case err != nil:
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		case !bytes.Equal(normalizeText(data), normalizeText([]byte(f.Content))):
			fp.State = FileDiffers
		}
		p.Files = append(p.Files, fp)
	}
	for _, s := range r.Setup {
		p.Setup = append(p.Setup, s.Name)
	}
	for _, s := range r.Services {
		p.Services = append(p.Services, s.Name)
		if s.Expose != nil && s.Expose.Auth == "key" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("service %s: auth key is not enforced yet: /dyn/%s is open to the whole tailnet", s.Name, s.Expose.Endpoint))
		}
	}
	return p, nil
}

func planArtifact(fsys fs.FS, a Artifact, verify bool, hash func(fs.FS, string) (string, error)) (ArtifactPlan, error) {
	ap := ArtifactPlan{ID: a.ID, Path: a.Path, Size: a.Size, Mirrors: len(a.URLs)}
	if a.Unpack != nil {
		ok, err := exists(fsys, a.Unpack.Creates)
		if err != nil {
			return ap, err
		}
		if ok {
			if ok, err = exists(fsys, UnpackMarker(a.ID)); err != nil {
				return ap, err
			}
			ok = !ok
		}
		if ok {
			ap.State = Unpacked
			return ap, nil
		}
		ap.Unpack = true
	}

	fi, err := fs.Stat(fsys, a.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		ap.State, ap.Fetch = Missing, true
		pi, err := fs.Stat(fsys, a.Path+".part")
		if err == nil && pi.Mode().IsRegular() && pi.Size() > 0 && (a.Size == 0 || pi.Size() <= a.Size) {
			ap.State, ap.Have = Partial, pi.Size()
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ap, fmt.Errorf("%s.part: %w", a.Path, err)
		}
		return ap, nil
	case err != nil:
		return ap, fmt.Errorf("%s: %w", a.Path, err)
	case !fi.Mode().IsRegular():
		ap.State, ap.Unpack = Conflict, false
		return ap, nil
	case a.Size != 0 && fi.Size() != a.Size:
		ap.State, ap.Fetch = WrongSize, true
		return ap, nil
	}

	ap.State, ap.Have = Present, fi.Size()
	if verify && a.SHA256 != "" {
		sum, err := hash(fsys, a.Path)
		if err != nil {
			return ap, fmt.Errorf("%s: %w", a.Path, err)
		}
		if sum != a.SHA256 {
			ap.State, ap.Fetch, ap.Have = WrongHash, true, 0
			return ap, nil
		}
		ap.Verified = true
	}
	return ap, nil
}

// Done reports whether applying the plan has nothing to download, unpack or
// write. Setup steps and services may still need to run.
func (p *Plan) Done() bool {
	for _, a := range p.Artifacts {
		if a.Fetch || a.Unpack || a.State == Conflict {
			return false
		}
	}
	for _, f := range p.Files {
		if f.State != FileSame {
			return false
		}
	}
	return true
}

// HashFile returns the hex sha256 of a file.
func HashFile(fsys fs.FS, name string) (string, error) {
	return hashFile(context.Background(), fsys, name)
}

func hashFile(ctx context.Context, fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader fails reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func exists(fsys fs.FS, name string) (bool, error) {
	_, err := fs.Stat(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// normalizeText drops a UTF-8 BOM and turns CRLF into LF, so a config that
// PowerShell or Notepad saved (same text, different bytes) is not rewritten.
func normalizeText(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

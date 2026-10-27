package recipe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Install states.
const (
	StateInstalling = "installing"
	StateInstalled  = "installed"
	StateFailed     = "failed"
	StateCanceled   = "canceled"
)

// Installed is what a machine knows about one recipe it has (or is
// installing). It survives agent restarts in installed.json, so each agent
// knows what it has even when the panel is off. The recipe is stored as
// written, never expanded: expanding would write secrets to disk.
type Installed struct {
	Name      string          `json:"name"`
	Version   string          `json:"version"`
	Profile   string          `json:"profile,omitempty"`
	State     string          `json:"state"`
	Phase     string          `json:"phase,omitempty"`
	Error     string          `json:"error,omitempty"`
	Recipe    json.RawMessage `json:"recipe"`
	StartedAt time.Time       `json:"started_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	// NeedsSecrets is set when the recipe uses secrets: an interrupted
	// install cannot resume by itself because secrets are not stored.
	NeedsSecrets bool `json:"needs_secrets,omitempty"`
}

// Store is installed.json. Safe for concurrent use.
type Store struct {
	mu   sync.Mutex
	path string
	m    map[string]*Installed
}

// OpenStore loads path (missing is fine: nothing installed yet). An empty
// path keeps everything in memory.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]*Installed{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Installed
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, in := range list {
		s.m[in.Name] = in
	}
	return s, nil
}

// Get returns a copy of the entry for name.
func (s *Store) Get(name string) (Installed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.m[name]
	if !ok {
		return Installed{}, false
	}
	return *in, true
}

// List returns every entry, sorted by name.
func (s *Store) List() []Installed {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Installed, 0, len(s.m))
	for _, in := range s.m {
		out = append(out, *in)
	}
	slices.SortFunc(out, func(a, b Installed) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Update changes (or creates) the entry for name and saves the file.
func (s *Store) Update(name string, fn func(*Installed)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.m[name]
	if !ok {
		in = &Installed{Name: name}
		s.m[name] = in
	}
	fn(in)
	in.UpdatedAt = time.Now().UTC()
	return s.saveLocked()
}

// Delete removes the entry for name.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return s.saveLocked()
}

// saveLocked writes the file atomically (temp file + rename), so a crash
// mid-write never leaves a truncated installed.json.
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	list := make([]*Installed, 0, len(s.m))
	for _, in := range s.m {
		list = append(list, in)
	}
	slices.SortFunc(list, func(a, b *Installed) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

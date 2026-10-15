package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The tasks live on each agent; the panel keeps the last copy it got, so the
// history (the audit) still shows while a machine is off. Served with
// stale:true and cached_at.
type workCache struct {
	mu      sync.Mutex
	path    string
	Lists   map[string]cachedList   `json:"lists"`   // machine → its recent tasks
	Details map[string]cachedDetail `json:"details"` // machine + "\x00" + id → work.get
}

type cachedList struct {
	Tasks []workTask `json:"tasks"`
	At    time.Time  `json:"at"`
}

type cachedDetail struct {
	Detail json.RawMessage `json:"detail"`
	At     time.Time       `json:"at"`
}

// maxCachedDetails bounds the task details kept (oldest dropped first).
const maxCachedDetails = 200

func workCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "astrea", "panel-work.json")
}

func loadWorkCache(path string) *workCache {
	c := &workCache{path: path, Lists: map[string]cachedList{}, Details: map[string]cachedDetail{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, c)
	}
	if c.Lists == nil {
		c.Lists = map[string]cachedList{}
	}
	if c.Details == nil {
		c.Details = map[string]cachedDetail{}
	}
	return c
}

func (c *workCache) putList(machine string, tasks []workTask) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Lists[machine] = cachedList{Tasks: tasks, At: time.Now()}
	c.save()
}

// patch refreshes one task in machine's cached list (after a cancel, or when
// its detail was read), keeping the copy shown while offline up to date.
func (c *workCache) patch(machine string, t workTask) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.Lists[machine]
	if !ok {
		return
	}
	for i := range l.Tasks {
		if l.Tasks[i].ID == t.ID {
			t.Notes, t.Machine = nil, machine
			l.Tasks[i] = t
			c.save()
			return
		}
	}
}

// list returns the cached tasks of machine marked stale.
func (c *workCache) list(machine string) ([]workTask, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.Lists[machine]
	if !ok {
		return nil, false
	}
	out := make([]workTask, len(l.Tasks))
	for i, t := range l.Tasks {
		t.Machine, t.Stale, t.CachedAt = machine, true, &l.At
		out[i] = t
	}
	return out, true
}

func (c *workCache) putDetail(machine, id string, detail any) {
	if c == nil {
		return
	}
	data, err := json.Marshal(detail)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Details[machine+"\x00"+id] = cachedDetail{Detail: data, At: time.Now()}
	for len(c.Details) > maxCachedDetails {
		oldest, at := "", time.Now()
		for k, d := range c.Details {
			if d.At.Before(at) {
				oldest, at = k, d.At
			}
		}
		delete(c.Details, oldest)
	}
	c.save()
}

func (c *workCache) detail(machine, id string) (cachedDetail, bool) {
	if c == nil {
		return cachedDetail{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.Details[machine+"\x00"+id]
	return d, ok
}

// forget drops a machine removed from the fleet.
func (c *workCache) forget(keep map[string]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for m := range c.Lists {
		if !keep[m] {
			delete(c.Lists, m)
			changed = true
		}
	}
	for k := range c.Details {
		if m, _, _ := strings.Cut(k, "\x00"); !keep[m] {
			delete(c.Details, k)
			changed = true
		}
	}
	if changed {
		c.save()
	}
}

func (c *workCache) save() {
	if c.path == "" {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.path), 0o700)
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

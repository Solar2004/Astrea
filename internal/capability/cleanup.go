package capability

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// Cleaner frees disk space by deleting what is safe to delete: old files in
// the temp folders and old logs of finished jobs. It runs on its own when the
// system disk passes AutoPercent, at most every 6 hours.
type Cleaner struct {
	JobsDir     string  // the agent's job logs
	AutoPercent float64 // 0 = never on its own

	mu      sync.Mutex
	last    *CleanupResult
	lastRun time.Time
}

// CleanupResult is what one pass did (or would do, on a dry run).
type CleanupResult struct {
	Time    time.Time `json:"time"`
	Auto    bool      `json:"auto,omitempty"`
	DryRun  bool      `json:"dry_run,omitempty"`
	Freed   int64     `json:"freed_bytes"`
	Files   int       `json:"files"`
	Skipped int       `json:"skipped"` // in use or no permission
	Disk    string    `json:"disk"`
	Before  float64   `json:"used_percent_before"`
	After   float64   `json:"used_percent_after,omitempty"`
}

type cleanTarget struct {
	dir string
	age time.Duration
}

// isTempDir guards against a temp path that falls back to the Windows or
// home folder when TEMP/TMP are unset: only folders named temp/tmp are swept.
func isTempDir(dir string) bool {
	switch strings.ToLower(filepath.Base(filepath.Clean(dir))) {
	case "temp", "tmp":
		return true
	}
	return false
}

func (c *Cleaner) targets(minAge time.Duration) []cleanTarget {
	var t []cleanTarget
	dirs := []string{os.TempDir()}
	if root := os.Getenv("SystemRoot"); runtime.GOOS == "windows" && root != "" {
		dirs = append(dirs, filepath.Join(root, "Temp"))
	}
	for _, d := range dirs {
		if isTempDir(d) {
			t = append(t, cleanTarget{d, minAge})
		}
	}
	if c.JobsDir != "" {
		t = append(t, cleanTarget{c.JobsDir, 14 * 24 * time.Hour})
	}
	return t
}

// systemDisk is the mount the OS lives on.
func systemDisk() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("SystemDrive"); d != "" {
			return d + `\`
		}
		return `C:\`
	}
	return "/"
}

func usedPercent(ctx context.Context, mount string) float64 {
	if u, err := disk.UsageWithContext(ctx, mount); err == nil {
		return u.UsedPercent
	}
	return 0
}

// Run does one pass. Files modified less than minAge ago are kept.
func (c *Cleaner) Run(ctx context.Context, minAge time.Duration, dryRun, auto bool) CleanupResult {
	res := CleanupResult{Time: time.Now().UTC(), Auto: auto, DryRun: dryRun, Disk: systemDisk()}
	res.Before = usedPercent(ctx, res.Disk)
	for _, t := range c.targets(minAge) {
		cut := time.Now().Add(-t.age)
		var dirs []string
		_ = filepath.WalkDir(t.dir, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				res.Skipped++
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if p != t.dir {
					dirs = append(dirs, p)
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || info.ModTime().After(cut) || !info.Mode().IsRegular() {
				return nil
			}
			if !dryRun {
				if os.Remove(p) != nil {
					res.Skipped++
					return nil
				}
			}
			res.Files++
			res.Freed += info.Size()
			return nil
		})
		if !dryRun {
			// Deepest first: remove the folders the pass left empty.
			for i := len(dirs) - 1; i >= 0; i-- {
				_ = os.Remove(dirs[i]) // fails (kept) unless empty
			}
		}
	}
	if !dryRun {
		res.After = usedPercent(ctx, res.Disk)
		c.mu.Lock()
		c.last, c.lastRun = &res, time.Now()
		c.mu.Unlock()
	}
	return res
}

// Last is the last real pass, if any.
func (c *Cleaner) Last() *CleanupResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Loop checks the system disk every hour and cleans when it is too full.
func (c *Cleaner) Loop(ctx context.Context) {
	if c.AutoPercent <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		recent := time.Since(c.lastRun) < 6*time.Hour
		c.mu.Unlock()
		if !recent && usedPercent(ctx, systemDisk()) >= c.AutoPercent {
			c.Run(ctx, 3*24*time.Hour, false, true)
		}
	}
}

// DiskCleanup exposes the Cleaner.
type DiskCleanup struct{ Cleaner *Cleaner }

func (DiskCleanup) Name() string { return "disk.cleanup" }

func (DiskCleanup) Describe() Descriptor {
	return Descriptor{
		Name: "disk.cleanup",
		Description: "Free disk space safely: delete old files in the temp folders (user and Windows) and logs of jobs older than 14 days. " +
			"Runs on its own when the system disk passes 90%. dry_run reports what would be freed; the result includes the last automatic pass.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"dry_run": {"type": "boolean", "description": "only report, delete nothing"},
				"min_age_days": {"type": "integer", "description": "keep temp files newer than this, default 3"}
			}
		}`),
	}
}

func (c DiskCleanup) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	var req struct {
		DryRun     bool `json:"dry_run"`
		MinAgeDays *int `json:"min_age_days"`
	}
	if err := decode(payload, &req); err != nil {
		return nil, err
	}
	age := 3
	if req.MinAgeDays != nil && *req.MinAgeDays >= 0 {
		age = *req.MinAgeDays
	}
	prev := c.Cleaner.Last()
	res := c.Cleaner.Run(ctx, time.Duration(age)*24*time.Hour, req.DryRun, false)
	return map[string]any{"result": res, "previous": prev}, nil
}

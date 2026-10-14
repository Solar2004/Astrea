package supervise

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Job states.
const (
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
	JobTimedOut  = "timed_out"
)

// keepJobs is how many finished jobs (and their logs) are remembered.
const keepJobs = 100

// Job is a one-shot background command.
type Job struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	Command   Command   `json:"command"`
	State     string    `json:"state"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// DurationSec is wall time so far (running) or total (finished).
	DurationSec float64 `json:"duration_sec"`
	LogPath     string  `json:"log_path"`

	proc     *process
	finished chan struct{} // closed once State is final
	canceled bool
	timedOut bool
}

// Jobs runs and tracks background jobs. Jobs do not survive an agent restart.
type Jobs struct {
	dir  string
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewJobs keeps job logs under dir.
func NewJobs(dir string) *Jobs {
	return &Jobs{dir: dir, jobs: map[string]*Job{}}
}

// Start launches c in the background. maxRuntime > 0 kills it after that long.
func (m *Jobs) Start(c Command, label string, maxRuntime time.Duration) (Job, error) {
	if c.Command == "" {
		return Job{}, fmt.Errorf("command is required")
	}
	id := "j-" + uuid.NewString()[:8]
	j := &Job{ID: id, Label: label, Command: c, State: JobRunning, StartedAt: time.Now(),
		LogPath: filepath.Join(m.dir, id+".log"), finished: make(chan struct{})}
	p, err := spawn(c, j.LogPath)
	if err != nil {
		return Job{}, err
	}
	j.proc = p

	m.mu.Lock()
	m.jobs[id] = j
	m.pruneLocked()
	m.mu.Unlock()

	go m.watch(j, maxRuntime)
	return m.snapshot(j), nil
}

func (m *Jobs) watch(j *Job, maxRuntime time.Duration) {
	var timeout <-chan time.Time
	if maxRuntime > 0 {
		t := time.NewTimer(maxRuntime)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-j.proc.done:
	case <-timeout:
		m.mu.Lock()
		j.timedOut = true
		m.mu.Unlock()
		j.proc.kill()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(j.finished)
	code := j.proc.exitCode
	j.ExitCode = &code
	j.EndedAt = time.Now()
	switch {
	case j.canceled:
		j.State = JobCanceled
	case j.timedOut:
		j.State = JobTimedOut
	case j.proc.err != nil:
		j.State, j.Error = JobFailed, j.proc.err.Error()
	case code == 0:
		j.State = JobSucceeded
	default:
		j.State = JobFailed
	}
}

// pruneLocked forgets the oldest finished jobs beyond keepJobs.
func (m *Jobs) pruneLocked() {
	if len(m.jobs) <= keepJobs {
		return
	}
	var done []*Job
	for _, j := range m.jobs {
		if j.State != JobRunning {
			done = append(done, j)
		}
	}
	sort.Slice(done, func(a, b int) bool { return done[a].StartedAt.Before(done[b].StartedAt) })
	for _, j := range done[:max(0, min(len(done), len(m.jobs)-keepJobs))] {
		delete(m.jobs, j.ID)
		_ = os.Remove(j.LogPath)
	}
}

func (m *Jobs) snapshot(j *Job) Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(j)
}

func (m *Jobs) snapshotLocked(j *Job) Job {
	s := *j
	s.proc, s.finished = nil, nil
	end := j.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	s.DurationSec = end.Sub(j.StartedAt).Round(time.Second).Seconds()
	return s
}

func (m *Jobs) get(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("no job %q", id)
	}
	return j, nil
}

// Get returns a job, waiting up to wait for it to finish if it is running.
func (m *Jobs) Get(ctx context.Context, id string, wait time.Duration) (Job, error) {
	j, err := m.get(id)
	if err != nil {
		return Job{}, err
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-j.finished:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return m.snapshot(j), nil
}

// List returns all remembered jobs, newest first.
func (m *Jobs) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, m.snapshotLocked(j))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// Cancel kills a running job's whole process tree.
func (m *Jobs) Cancel(id string) (Job, error) {
	j, err := m.get(id)
	if err != nil {
		return Job{}, err
	}
	m.mu.Lock()
	running := j.State == JobRunning
	if running {
		j.canceled = true
	}
	m.mu.Unlock()
	if running {
		j.proc.kill()
	}
	return m.Get(context.Background(), id, time.Second)
}

// Log reads a job's log (see ReadLog).
func (m *Jobs) Log(id string, offset int64, tailLines, maxBytes int) (LogChunk, error) {
	j, err := m.get(id)
	if err != nil {
		return LogChunk{}, err
	}
	return ReadLog(j.LogPath, offset, tailLines, maxBytes)
}

// StopAll kills every running job (agent shutdown).
func (m *Jobs) StopAll() {
	m.mu.Lock()
	var running []*Job
	for _, j := range m.jobs {
		if j.State == JobRunning {
			j.canceled = true
			running = append(running, j)
		}
	}
	m.mu.Unlock()
	for _, j := range running {
		j.proc.kill()
	}
}

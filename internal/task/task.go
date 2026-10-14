// Package task implements AI-defined automations on the controlled machine.
//
// A task is a named, parameterized unit of work the AI defines at runtime and
// then calls like any built-in capability: "what time is it here", "list the
// installed local models", "open Roblox and screenshot the game list", "queue
// this ComfyUI workflow". Two kinds:
//
//   - script: a script file (PowerShell, Python, bash, cmd, node) run with the
//     task's arguments in its environment and on stdin.
//   - macro: a list of capability calls (exec, sleep, mouse.*, screen.capture,
//     other tasks…) with {{param}} templating in their arguments.
//
// Tasks persist across agent restarts. This package knows nothing about the
// capability registry: callers inject a Dispatch func used by macro steps.
package task

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/protocol"
	"github.com/Solar2004/Astrea/internal/shell"
)

// Kind enumerates task kinds.
type Kind string

const (
	KindScript Kind = "script"
	KindMacro  Kind = "macro"
)

// Spec is one task definition.
type Spec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Params is the JSON schema (type object) of the task's arguments. It is
	// advertised as the tool's input schema; "default" values are applied.
	Params json.RawMessage `json:"params,omitempty"`
	Kind   Kind            `json:"kind"`

	// Script kind.
	Shell  string `json:"shell,omitempty"`
	Script string `json:"script,omitempty"`
	Cwd    string `json:"cwd,omitempty"`

	// Macro kind.
	Steps []Step `json:"steps,omitempty"`

	// TimeoutSec bounds a run. 0 = 300s.
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Updated    string `json:"updated,omitempty"`
}

// Step is one macro step: a capability call, or the built-in "sleep".
type Step struct {
	Call string          `json:"call"`
	Args json.RawMessage `json:"args,omitempty"`
	// ContinueOnError keeps the macro running if this step fails.
	ContinueOnError bool `json:"continue_on_error,omitempty"`
}

// Emit receives streamed output, like capability.Emit.
type Emit func(protocol.StreamChunk)

// Dispatch runs a capability by name; macro steps go through it.
type Dispatch func(ctx context.Context, method string, payload json.RawMessage, emit Emit) (any, error)

// MaxDepth bounds tasks calling tasks (directly or through macros).
const MaxDepth = 8

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)

// Reserved names collide with the task.* management capabilities.
var reserved = map[string]bool{"define": true, "list": true, "get": true, "delete": true, "run": true}

// Manager stores tasks and runs them.
type Manager struct {
	dir      string // persists tasks.json and materialized script files
	dispatch Dispatch

	mu    sync.RWMutex
	tasks map[string]*Spec
}

// NewManager loads tasks from dir (if non-empty). dispatch may be set later
// with SetDispatch, before any macro runs.
func NewManager(dir string) *Manager {
	m := &Manager{dir: dir, tasks: map[string]*Spec{}}
	m.load()
	return m
}

// SetDispatch wires the capability dispatcher used by macro steps.
func (m *Manager) SetDispatch(d Dispatch) { m.dispatch = d }

// Validate checks a spec without storing it.
func Validate(s *Spec) error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("name must match %s", nameRe)
	}
	if reserved[s.Name] {
		return fmt.Errorf("name %q is reserved", s.Name)
	}
	if len(s.Params) > 0 {
		var schema struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(s.Params, &schema); err != nil || schema.Type != "object" {
			return fmt.Errorf(`params must be a JSON schema with "type": "object"`)
		}
	}
	switch s.Kind {
	case KindScript:
		if strings.TrimSpace(s.Script) == "" {
			return fmt.Errorf("script task requires script")
		}
		if _, _, err := shell.Script(s.Shell, "x"); err != nil {
			return err
		}
	case KindMacro:
		if len(s.Steps) == 0 {
			return fmt.Errorf("macro task requires steps")
		}
		for i, st := range s.Steps {
			if st.Call == "" {
				return fmt.Errorf("step %d: call is required", i)
			}
		}
	default:
		return fmt.Errorf("unknown kind %q (want script or macro)", s.Kind)
	}
	return nil
}

// Define validates and stores (or replaces) a task.
func (m *Manager) Define(s *Spec) error {
	if err := Validate(s); err != nil {
		return err
	}
	s.Updated = time.Now().UTC().Format(time.RFC3339)
	m.mu.Lock()
	m.tasks[s.Name] = s
	m.mu.Unlock()
	return m.save()
}

// Delete removes a task. Returns whether it existed.
func (m *Manager) Delete(name string) bool {
	m.mu.Lock()
	_, ok := m.tasks[name]
	delete(m.tasks, name)
	m.mu.Unlock()
	if ok {
		_ = m.save()
	}
	return ok
}

// Get returns a task by name.
func (m *Manager) Get(name string) (*Spec, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.tasks[name]
	return s, ok
}

// List returns all tasks sorted by name.
func (m *Manager) List() []*Spec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Spec, 0, len(m.tasks))
	for _, s := range m.tasks {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type depthKey struct{}

// Run executes a task with the given JSON object arguments.
func (m *Manager) Run(ctx context.Context, name string, args json.RawMessage, emit Emit) (any, error) {
	s, ok := m.Get(name)
	if !ok {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf("no such task %q", name)}
	}
	depth, _ := ctx.Value(depthKey{}).(int)
	if depth >= MaxDepth {
		return nil, &protocol.Error{Code: protocol.CodeDenied, Message: "task nesting too deep (a task calling itself?)"}
	}
	ctx = context.WithValue(ctx, depthKey{}, depth+1)
	if emit == nil {
		emit = func(protocol.StreamChunk) {}
	}
	params, err := withDefaults(s.Params, args)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	timeout := time.Duration(s.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if s.Kind == KindMacro {
		return m.runMacro(ctx, s, params, emit)
	}
	return m.runScript(ctx, s, params, emit)
}

// withDefaults decodes args (an object) and fills in schema defaults.
func withDefaults(schema, args json.RawMessage) (map[string]any, error) {
	params := map[string]any{}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &params); err != nil {
			return nil, fmt.Errorf("task arguments must be a JSON object: %v", err)
		}
	}
	var sc struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(schema) > 0 && json.Unmarshal(schema, &sc) == nil {
		for k, p := range sc.Properties {
			if _, ok := params[k]; !ok && p.Default != nil {
				params[k] = p.Default
			}
		}
		for _, k := range sc.Required {
			if _, ok := params[k]; !ok {
				return nil, fmt.Errorf("missing required argument %q", k)
			}
		}
	}
	return params, nil
}

// --- script ---

// scriptDir holds the per-run script files.
func (m *Manager) scriptDir() string {
	if m.dir == "" {
		return filepath.Join(os.TempDir(), "astrea-tasks")
	}
	return filepath.Join(m.dir, "tasks")
}

type scriptResult struct {
	ExitCode int  `json:"exit_code"`
	Killed   bool `json:"killed,omitempty"`
	// Result is whatever the script wrote to $ASTREA_RESULT, if valid JSON.
	Result json.RawMessage `json:"result,omitempty"`
}

func (m *Manager) runScript(ctx context.Context, s *Spec, params map[string]any, emit Emit) (any, error) {
	// One file per run: concurrent runs never share (or lock, on Windows) a
	// script file, and the interpreter sees the extension it requires.
	if err := os.MkdirAll(m.scriptDir(), 0o700); err != nil {
		return nil, internal(err)
	}
	ext := shell.Ext(s.Shell)
	f, err := os.CreateTemp(m.scriptDir(), s.Name+"-*"+ext)
	if err != nil {
		return nil, internal(err)
	}
	path := f.Name()
	defer os.Remove(path)
	if ext == ".ps1" {
		// Windows PowerShell 5.1 reads BOM-less files as ANSI and mangles
		// accents; the BOM makes it read UTF-8.
		_, _ = f.WriteString("\ufeff")
	}
	_, err = f.WriteString(s.Script)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, internal(err)
	}
	resultFile, err := os.CreateTemp("", "astrea-result-*.json")
	if err != nil {
		return nil, internal(err)
	}
	resultFile.Close()
	defer os.Remove(resultFile.Name())

	argsJSON, _ := json.Marshal(params)
	name, argv, err := shell.Script(s.Shell, path)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: err.Error()}
	}
	cmd := exec.CommandContext(ctx, name, argv...)
	procutil.NoConsole(cmd)
	cmd.Dir = s.Cwd
	cmd.Stdin = strings.NewReader(string(argsJSON))
	cmd.Env = append(os.Environ(), "ASTREA_ARGS="+string(argsJSON), "ASTREA_RESULT="+resultFile.Name(), "ASTREA_TASK="+s.Name)
	for k, v := range params {
		cmd.Env = append(cmd.Env, "ASTREA_ARG_"+envName(k)+"="+scalar(v))
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, internal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pump(&wg, stdout, "stdout", emit)
	go pump(&wg, stderr, "stderr", emit)
	wg.Wait()
	err = cmd.Wait()

	res := scriptResult{Killed: ctx.Err() != nil}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	} else if err != nil && !res.Killed {
		return nil, internal(err)
	}
	if data, e := os.ReadFile(resultFile.Name()); e == nil {
		data = []byte(strings.TrimPrefix(strings.TrimSpace(string(data)), "\ufeff"))
		if len(data) > 0 && json.Valid(data) {
			res.Result = data
		}
	}
	return liftImages(res), nil
}

// liftImages moves an "images" array out of a script's result to the top
// level, where the hub renders it as image content.
func liftImages(res scriptResult) any {
	out := map[string]any{"exit_code": res.ExitCode}
	if res.Killed {
		out["killed"] = true
	}
	if len(res.Result) == 0 {
		return out
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(res.Result, &obj) == nil {
		if imgs, ok := obj["images"]; ok {
			out["images"] = imgs
			delete(obj, "images")
		}
		out["result"] = obj
		return out
	}
	out["result"] = res.Result
	return out
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func envName(k string) string { return strings.ToUpper(nonAlnum.ReplaceAllString(k, "_")) }

// scalar renders a value for an environment variable: strings verbatim,
// anything else as JSON.
func scalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func pump(wg *sync.WaitGroup, r io.Reader, channel string, emit Emit) {
	defer wg.Done()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		emit(protocol.StreamChunk{Channel: channel, Data: sc.Text() + "\n"})
	}
}

// --- macro ---

type stepResult struct {
	Call   string `json:"call"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (m *Manager) runMacro(ctx context.Context, s *Spec, params map[string]any, emit Emit) (any, error) {
	if m.dispatch == nil {
		return nil, internal(errors.New("macro dispatcher not configured"))
	}
	var steps []stepResult
	var images []json.RawMessage
	for i, st := range s.Steps {
		args, err := render(st.Args, params)
		if err != nil {
			return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: fmt.Sprintf("step %d (%s): %v", i, st.Call, err)}
		}
		emit(protocol.StreamChunk{Channel: "astrea", Data: fmt.Sprintf("▶ step %d: %s\n", i, st.Call)})
		var res any
		if st.Call == "sleep" {
			err = sleep(ctx, args)
		} else {
			res, err = m.dispatch(ctx, st.Call, args, emit)
		}
		sr := stepResult{Call: st.Call, OK: err == nil}
		if err != nil {
			sr.Error = err.Error()
		} else {
			var imgs []json.RawMessage
			sr.Result, imgs = stripImages(res)
			images = append(images, imgs...)
		}
		steps = append(steps, sr)
		if err != nil && !st.ContinueOnError {
			out := map[string]any{"steps": steps, "failed_step": i}
			if len(images) > 0 {
				out["images"] = images
			}
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, &protocol.Error{Code: protocol.CodeInternal, Message: "task timed out or was cancelled"}
		}
	}
	out := map[string]any{"steps": steps}
	if len(images) > 0 {
		out["images"] = images
	}
	return out, nil
}

// stripImages removes a top-level "images" array from a step result so the
// macro can surface every captured image once, at its own top level.
func stripImages(res any) (any, []json.RawMessage) {
	b, err := json.Marshal(res)
	if err != nil {
		return res, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return json.RawMessage(b), nil
	}
	var imgs []json.RawMessage
	if raw, ok := obj["images"]; ok {
		_ = json.Unmarshal(raw, &imgs)
		delete(obj, "images")
	}
	return obj, imgs
}

func sleep(ctx context.Context, args json.RawMessage) error {
	var p struct {
		MS  int     `json:"ms"`
		Sec float64 `json:"sec"`
	}
	_ = json.Unmarshal(args, &p)
	d := time.Duration(p.MS)*time.Millisecond + time.Duration(p.Sec*float64(time.Second))
	if d <= 0 || d > 10*time.Minute {
		return fmt.Errorf("sleep needs ms or sec, up to 10 minutes")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

var tmplRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// render substitutes {{param}} in every string of a step's arguments. A string
// that is exactly "{{param}}" takes the parameter's JSON value (so numbers stay
// numbers); otherwise the value is interpolated as text.
func render(raw json.RawMessage, params map[string]any) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			if m := tmplRe.FindStringSubmatch(t); m != nil && m[0] == strings.TrimSpace(t) {
				return params[m[1]]
			}
			return tmplRe.ReplaceAllStringFunc(t, func(s string) string {
				v, ok := params[tmplRe.FindStringSubmatch(s)[1]]
				if !ok || v == nil {
					return ""
				}
				return scalar(v)
			})
		case map[string]any:
			for k, e := range t {
				t[k] = walk(e)
			}
		case []any:
			for i, e := range t {
				t[i] = walk(e)
			}
		}
		return x
	}
	return json.Marshal(walk(v))
}

// --- persistence ---

func (m *Manager) storePath() string { return filepath.Join(m.dir, "tasks.json") }

func (m *Manager) load() {
	if m.dir == "" {
		return
	}
	data, err := os.ReadFile(m.storePath())
	if err != nil {
		return
	}
	var specs []*Spec
	if json.Unmarshal(data, &specs) != nil {
		return
	}
	for _, s := range specs {
		if Validate(s) == nil {
			m.tasks[s.Name] = s
		}
	}
}

func (m *Manager) save() error {
	if m.dir == "" {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.List(), "", "  ")
	if err != nil {
		return err
	}
	// 0600: tasks hold scripts the agent will run.
	return os.WriteFile(m.storePath(), data, 0o600)
}

func internal(err error) error {
	return &protocol.Error{Code: protocol.CodeInternal, Message: err.Error()}
}

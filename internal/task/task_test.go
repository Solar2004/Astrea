package task

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/Solar2004/Astrea/internal/protocol"
)

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestScriptArgsOutputAndResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	m := NewManager(t.TempDir())
	err := m.Define(&Spec{
		Name:   "greet",
		Kind:   KindScript,
		Shell:  "sh",
		Params: json.RawMessage(`{"type":"object","properties":{"who":{"type":"string","default":"mundo"},"n":{"type":"integer"}}}`),
		Script: `echo "hola $ASTREA_ARG_WHO"
printf '{"n": %s, "images": [{"mime_type":"image/png","data":"aGk="}]}' "$ASTREA_ARG_N" > "$ASTREA_RESULT"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	res, err := m.Run(context.Background(), "greet", json.RawMessage(`{"n": 3}`), func(c protocol.StreamChunk) { out.WriteString(c.Data) })
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hola mundo\n" {
		t.Fatalf("stdout = %q (default param not applied?)", out.String())
	}
	r := asMap(t, res)
	if r["exit_code"].(float64) != 0 || r["result"].(map[string]any)["n"].(float64) != 3 {
		t.Fatalf("unexpected result: %v", r)
	}
	if _, ok := r["images"]; !ok {
		t.Fatalf("images should be lifted to the top level: %v", r)
	}
}

func TestMacroTemplatingAndImages(t *testing.T) {
	m := NewManager("")
	var calls []string
	m.SetDispatch(func(_ context.Context, method string, payload json.RawMessage, _ Emit) (any, error) {
		calls = append(calls, method+" "+string(payload))
		if method == "screen.capture" {
			return map[string]any{"scale": 1, "images": []map[string]string{{"mime_type": "image/jpeg", "data": "eA=="}}}, nil
		}
		return map[string]any{"ok": true}, nil
	})
	err := m.Define(&Spec{
		Name:   "open_and_look",
		Kind:   KindMacro,
		Params: json.RawMessage(`{"type":"object","properties":{"app":{"type":"string"},"x":{"type":"integer"}},"required":["app"]}`),
		Steps: []Step{
			{Call: "exec", Args: json.RawMessage(`{"command":"start {{app}}","shell":"cmd"}`)},
			{Call: "sleep", Args: json.RawMessage(`{"ms":1}`)},
			{Call: "mouse.move", Args: json.RawMessage(`{"x":"{{x}}","y":10}`)},
			{Call: "screen.capture"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(context.Background(), "open_and_look", json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("missing required arg should fail")
	}
	res, err := m.Run(context.Background(), "open_and_look", json.RawMessage(`{"app":"roblox","x":42}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`exec {"command":"start roblox","shell":"cmd"}`,
		`mouse.move {"x":42,"y":10}`,
		`screen.capture {}`,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	r := asMap(t, res)
	if imgs, _ := r["images"].([]any); len(imgs) != 1 {
		t.Fatalf("macro should surface the captured image: %v", r)
	}
	if strings.Contains(mustJSON(r["steps"]), "eA==") {
		t.Fatal("image data should not be duplicated inside step results")
	}
}

func TestMacroStopsOnError(t *testing.T) {
	m := NewManager("")
	m.SetDispatch(func(_ context.Context, method string, _ json.RawMessage, _ Emit) (any, error) {
		if method == "boom" {
			return nil, &protocol.Error{Code: "denied", Message: "nope"}
		}
		return nil, nil
	})
	_ = m.Define(&Spec{Name: "m", Kind: KindMacro, Steps: []Step{{Call: "boom"}, {Call: "never"}}})
	r := asMap(t, mustRun(t, m, "m"))
	if r["failed_step"].(float64) != 0 || len(r["steps"].([]any)) != 1 {
		t.Fatalf("macro should stop at the failing step: %v", r)
	}
}

func TestNestingIsBounded(t *testing.T) {
	m := NewManager("")
	m.SetDispatch(func(ctx context.Context, method string, payload json.RawMessage, emit Emit) (any, error) {
		return m.Run(ctx, strings.TrimPrefix(method, "task."), payload, emit)
	})
	_ = m.Define(&Spec{Name: "loop", Kind: KindMacro, Steps: []Step{{Call: "task.loop"}}})
	if !strings.Contains(mustJSON(mustRun(t, m, "loop")), "too deep") {
		t.Fatal("self-recursive task should hit the depth limit")
	}
}

func TestPersistenceAndValidation(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	if err := m.Define(&Spec{Name: "hora", Kind: KindScript, Script: "date"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := NewManager(dir).Get("hora"); !ok {
		t.Fatal("task should survive a restart")
	}
	for _, bad := range []*Spec{
		{Name: "bad name", Kind: KindScript, Script: "x"},
		{Name: "define", Kind: KindScript, Script: "x"},
		{Name: "x", Kind: KindScript},
		{Name: "x", Kind: KindMacro},
		{Name: "x", Kind: KindScript, Script: "x", Shell: "zsh-nope"},
		{Name: "x", Kind: KindScript, Script: "x", Params: json.RawMessage(`{"type":"string"}`)},
	} {
		if err := m.Define(bad); err == nil {
			t.Errorf("spec %+v should be rejected", bad)
		}
	}
	if !m.Delete("hora") || m.Delete("hora") {
		t.Fatal("delete should report existence")
	}
}

func mustRun(t *testing.T, m *Manager, name string) any {
	t.Helper()
	res, err := m.Run(context.Background(), name, nil, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return res
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

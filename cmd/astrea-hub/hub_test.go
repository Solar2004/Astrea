package main

import (
	"encoding/json"
	"log"
	"testing"

	"github.com/Solar2004/Astrea/internal/fleet"
	"github.com/Solar2004/Astrea/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testHub(t *testing.T, machines ...string) *hub {
	t.Helper()
	var ms []fleet.Machine
	for _, name := range machines {
		ms = append(ms, fleet.Machine{Name: name, WS: "ws://127.0.0.1:7700/ws", Token: "t"})
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t"}, nil)
	return newHub(srv, fleet.New(ms), "astrea.", t.TempDir(), 0, log.Default())
}

func TestWithMachineParamSingleMachine(t *testing.T) {
	// One machine: no "machine" param added.
	out := withMachineParam(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`), []string{"win"}, false)
	if contains(string(out), "machine") {
		t.Fatalf("single machine should not add a machine param: %s", out)
	}
}

func TestWithMachineParamMultiRequiredWhenShared(t *testing.T) {
	out := withMachineParam(json.RawMessage(`{"type":"object","properties":{}}`), []string{"a", "b"}, true)
	var s map[string]any
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	props := s["properties"].(map[string]any)
	if _, ok := props["machine"]; !ok {
		t.Fatal("machine param missing")
	}
	req, _ := s["required"].([]any)
	if len(req) != 1 || req[0] != "machine" {
		t.Fatalf("machine should be required when shared: %v", req)
	}
	// Owned by one machine only: param present (enum of 1) but not required.
	out = withMachineParam(json.RawMessage(`{"type":"object"}`), []string{"a"}, true)
	var s2 map[string]any
	_ = json.Unmarshal(out, &s2)
	if _, ok := s2["required"]; ok {
		t.Fatalf("single owner should not force machine required: %s", out)
	}
}

func TestRoute(t *testing.T) {
	h := testHub(t, "a", "b")
	h.caps["a"] = []transport.CapDescriptor{{Name: "exec"}, {Name: "task.hora"}}
	h.caps["b"] = []transport.CapDescriptor{{Name: "exec"}}
	h.apply()

	// Shared cap without machine -> error listing owners.
	if _, _, _, err := h.route("exec", json.RawMessage(`{"work":"continue"}`)); err == nil {
		t.Fatal("exec on two machines needs an explicit machine")
	}
	// Explicit machine; machine and work are stripped from forwarded args.
	n, args, spec, err := h.route("exec", json.RawMessage(`{"machine":"b","command":"echo hi","work":{"title":"t"}}`))
	if err != nil || n.Name != "b" || contains(string(args), "machine") || contains(string(args), "work") || string(spec) != `{"title":"t"}` {
		t.Fatalf("route exec@b: %v %s %s %s", err, n.Name, args, spec)
	}
	// Cap owned by one machine: routed without an explicit machine.
	n, _, _, err = h.route("task.hora", json.RawMessage(`{"work":"continue"}`))
	if err != nil || n.Name != "a" {
		t.Fatalf("task.hora should route to its only owner a: %v", err)
	}
	// Unknown machine.
	if _, _, _, err := h.route("exec", json.RawMessage(`{"machine":"z","work":"continue"}`)); err == nil {
		t.Fatal("unknown machine should fail")
	}
}

func TestWorkRequired(t *testing.T) {
	h := testHub(t, "a")
	// A mutating call must declare its task.
	if _, _, _, err := h.route("exec", json.RawMessage(`{"command":"ls"}`)); err == nil || !contains(err.Error(), "work") {
		t.Fatalf("exec without work should fail: %v", err)
	}
	// Looking needs none, and a stray work is not forwarded.
	if _, _, spec, err := h.route("sys.stats", json.RawMessage(`{"work":"continue"}`)); err != nil || spec != nil {
		t.Fatalf("sys.stats: %v %s", err, spec)
	}
	for _, c := range []string{"work.start", "admin.lock", "agent.update", "screen.capture"} {
		if needsWork(c) {
			t.Errorf("%s should not need work", c)
		}
	}
	var s map[string]any
	_ = json.Unmarshal(withWorkParam(json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)), &s)
	if req, _ := s["required"].([]any); len(req) != 2 || req[1] != "work" {
		t.Fatalf("work should be required: %v", s["required"])
	}
}

func TestGatewayResolve(t *testing.T) {
	g := &gateway{models: map[string]model{
		"a/llm": {id: "a/llm", machine: "a", endpoint: "llm"},
		"b/llm": {id: "b/llm", machine: "b", endpoint: "llm"},
		"a/sd":  {id: "a/sd", machine: "a", endpoint: "sd"},
	}}
	// Exact id with a local model name after ':'.
	m, local, err := g.resolve("a/llm:qwen2.5")
	if err != nil || m.machine != "a" || local != "qwen2.5" {
		t.Fatalf("exact+local: %v %+v %q", err, m, local)
	}
	// Endpoint shared across machines -> ambiguous.
	if _, _, err := g.resolve("llm"); err == nil {
		t.Fatal("shared endpoint should be ambiguous")
	}
	// Endpoint unique across machines -> resolves.
	if m, _, err := g.resolve("sd"); err != nil || m.id != "a/sd" {
		t.Fatalf("unique endpoint: %v %+v", err, m)
	}
	if _, _, err := g.resolve("nope"); err == nil {
		t.Fatal("unknown model should fail")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

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
	if _, _, err := h.route("exec", json.RawMessage(`{}`)); err == nil {
		t.Fatal("exec on two machines needs an explicit machine")
	}
	// Explicit machine, and the machine arg is stripped from forwarded args.
	n, args, err := h.route("exec", json.RawMessage(`{"machine":"b","command":"echo hi"}`))
	if err != nil || n.Name != "b" || contains(string(args), "machine") {
		t.Fatalf("route exec@b: %v %s %s", err, n.Name, args)
	}
	// Cap owned by one machine: routed without an explicit machine.
	n, _, err = h.route("task.hora", json.RawMessage(`{}`))
	if err != nil || n.Name != "a" {
		t.Fatalf("task.hora should route to its only owner a: %v", err)
	}
	// Unknown machine.
	if _, _, err := h.route("exec", json.RawMessage(`{"machine":"z"}`)); err == nil {
		t.Fatal("unknown machine should fail")
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

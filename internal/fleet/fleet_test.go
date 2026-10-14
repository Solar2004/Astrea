package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"ws://100.1.2.3:7700/ws": "http://100.1.2.3:7700",
		"wss://host:8443/ws":     "https://host:8443",
	}
	for in, want := range cases {
		got, err := BaseURL(in)
		if err != nil || got != want {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://x/ws", "100.1.2.3:7700", ""} {
		if _, err := BaseURL(bad); err == nil {
			t.Errorf("BaseURL(%q) should fail", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := []Machine{{Name: "win", WS: "ws://100.1.2.3:7700/ws"}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	bad := [][]Machine{
		{},
		{{Name: "a b", WS: "ws://h:1/ws"}},
		{{Name: "x", WS: "ws://h:1/ws"}, {Name: "x", WS: "ws://h:2/ws"}},
		{{Name: "x", WS: "nope"}},
	}
	for i, ms := range bad {
		if err := Validate(ms); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestLoadFallsBackToPanelJSON(t *testing.T) {
	dir := t.TempDir()
	// No machines.json, but a panel.json exists next to it.
	panel := filepath.Join(dir, "panel.json")
	os.WriteFile(panel, []byte(`{"machines":[{"name":"win","ws":"ws://100.1.2.3:7700/ws","token":"t"}]}`), 0o600)
	ms, err := Load(filepath.Join(dir, "machines.json"))
	if err != nil || len(ms) != 1 || ms[0].Name != "win" {
		t.Fatalf("load fallback: %v %+v", err, ms)
	}
	// Neither file: ErrNoConfig.
	if _, err := Load(filepath.Join(t.TempDir(), "machines.json")); err != ErrNoConfig {
		t.Fatalf("want ErrNoConfig, got %v", err)
	}
}

func TestStatusMarshalsDesktopFlag(t *testing.T) {
	// A headless node: online, but Desktop=false and no IdleSec.
	no := false
	st := Status{Name: "pc", Online: true, Desktop: &no}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"desktop":false`) {
		t.Fatalf("desktop flag not surfaced: %s", b)
	}
	if strings.Contains(string(b), `"idle_sec"`) {
		t.Fatalf("headless node must not report idle_sec: %s", b)
	}
}

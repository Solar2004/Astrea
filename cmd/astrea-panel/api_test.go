package main

import (
	"testing"
	"time"
)

func TestParseCard(t *testing.T) {
	const tok = "e1bb0c37a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a225f"
	cases := []struct{ name, text, wantName, wantWS string }{
		{"hub-config.txt", `  astrea:
    command: /home/TU_USUARIO/.local/bin/astrea-hub
    args:
      - "--ws"
      - "ws://100.81.60.38:7700/ws"
      - "--caps"
      - "http://100.81.60.38:7700/capabilities"
      - "--token"
      - "` + tok + `"
    enabled: true
astrea-panel --ws ws://100.81.60.38:7700/ws --token ` + tok, "", "ws://100.81.60.38:7700/ws"},
		{"add-machine", "install.sh add-machine pc-cuarto 100.65.90.69 " + tok, "pc-cuarto", "ws://100.65.90.69:7700/ws"},
		{"json", `{"name":"x1","ws":"ws://10.0.0.2:7700/ws","token":"` + tok + `"}`, "x1", "ws://10.0.0.2:7700/ws"},
	}
	for _, c := range cases {
		name, ws, token := parseCard(c.text)
		if name != c.wantName || ws != c.wantWS || token != tok {
			t.Errorf("%s: got (%q, %q, %q)", c.name, name, ws, token)
		}
	}
	if _, ws, token := parseCard("add-machine prueba 100.64.0.99 abc"); ws != "ws://100.64.0.99:7700/ws" || token != "abc" {
		t.Errorf("short-token line parsed as %q %q", ws, token)
	}
	if _, ws, token := parseCard("hola"); ws != "" || token != "" {
		t.Errorf("garbage parsed as %q %q", ws, token)
	}
}

func TestCleanName(t *testing.T) {
	if got := cleanName("DESKTOP AJGOOT0.local"); got != "DESKTOP-AJGOOT0-local" {
		t.Fatal(got)
	}
}

func TestRingWrapsOldestFirst(t *testing.T) {
	var r ring
	for i := 0; i < historyLen+5; i++ {
		r.add(float64(i), 0, -1, time.Unix(int64(i), 0))
	}
	h := r.snapshot()
	if len(h.CPU) != historyLen || h.CPU[0] != 5 || h.CPU[historyLen-1] != float64(historyLen+4) {
		t.Fatalf("len %d first %v last %v", len(h.CPU), h.CPU[0], h.CPU[len(h.CPU)-1])
	}
}

func TestParsePing(t *testing.T) {
	q := parsePing("pong from desktop-0tvf884 (100.125.105.40) via 193.236.76.248:1450 in 21ms\n")
	if !q.Direct || q.MS != 21 || q.Via != "193.236.76.248:1450" {
		t.Errorf("direct = %+v", q)
	}
	q = parsePing("pong from f3-1 (100.1.2.3) via DERP(mad) in 112ms\n")
	if q.Direct || q.MS != 112 || q.Via != "mad" {
		t.Errorf("relay = %+v", q)
	}
	if q = parsePing("ping \"100.65.90.69\" timed out\nno reply\n"); q.Error == "" {
		t.Errorf("timeout = %+v", q)
	}
}

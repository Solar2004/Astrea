package main

import "testing"

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
	if _, ws, token := parseCard("hola"); ws != "" || token != "" {
		t.Errorf("garbage parsed as %q %q", ws, token)
	}
}

func TestCleanName(t *testing.T) {
	if got := cleanName("DESKTOP AJGOOT0.local"); got != "DESKTOP-AJGOOT0-local" {
		t.Fatal(got)
	}
}

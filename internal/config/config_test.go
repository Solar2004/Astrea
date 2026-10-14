package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := append([]byte("\xef\xbb\xbf"), `{"addr":"100.1.2.3:7700","token":"abc"}`...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load with BOM: %v", err)
	}
	if c.Addr != "100.1.2.3:7700" || c.Token != "abc" {
		t.Fatalf("got %+v", c)
	}
}

func TestEffectiveAllow(t *testing.T) {
	if got := (&Config{}).EffectiveAllow(); len(got) != 0 {
		t.Fatalf("yolo (default) must not restrict anything, got %v", got)
	}
	ro := (&Config{Mode: ModeReadOnly, Allow: []string{"task.hora"}}).EffectiveAllow()
	has := map[string]bool{}
	for _, m := range ro {
		has[m] = true
	}
	if !has["fs.read"] || !has["task.hora"] || has["exec"] {
		t.Fatalf("readonly allow list wrong: %v", ro)
	}
	if typo := (&Config{Mode: "yollo"}).EffectiveAllow(); len(typo) == 0 {
		t.Fatal("an unknown mode must fall back to readonly, not to yolo")
	}
}

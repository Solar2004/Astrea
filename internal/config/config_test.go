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

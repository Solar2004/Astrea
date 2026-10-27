package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardAllowDeny(t *testing.T) {
	g := NewGuard([]string{"exec", "fs.read"}, []string{"proc.kill"})

	if err := g.Check("exec"); err != nil {
		t.Fatalf("exec should be allowed: %v", err)
	}
	if err := g.Check("fs.write"); err == nil {
		t.Fatal("fs.write should be rejected (not in allow list)")
	}
	if err := g.Check("proc.kill"); err == nil {
		t.Fatal("proc.kill should be denied")
	}
	// admin.* bypasses the allow list so the operator never locks themselves out.
	if err := g.Check("admin.status"); err != nil {
		t.Fatalf("admin.status should always be allowed: %v", err)
	}
}

func TestGuardKillSwitch(t *testing.T) {
	g := NewGuard(nil, nil)
	if err := g.Check("exec"); err != nil {
		t.Fatalf("exec allowed before lock: %v", err)
	}
	g.SetLock(true)
	if err := g.Check("exec"); err == nil {
		t.Fatal("exec should be denied while locked")
	}
	if err := g.Check("admin.lock"); err != nil {
		t.Fatalf("admin.lock must work while locked: %v", err)
	}
	g.SetLock(false)
	if err := g.Check("exec"); err != nil {
		t.Fatalf("exec allowed after unlock: %v", err)
	}
}

func TestGuardWildcards(t *testing.T) {
	g := NewGuard([]string{"task.*", "screen.capture"}, []string{"task.danger"})
	if err := g.Check("task.hora"); err != nil {
		t.Fatalf("task.* should allow task.hora: %v", err)
	}
	if err := g.Check("task.danger"); err == nil {
		t.Fatal("explicit deny must win over a wildcard allow")
	}
	if err := g.Check("exec"); err == nil {
		t.Fatal("exec is not covered by task.*")
	}
}

func TestReadAuditNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	a.Log(context.Background(), "exec", json.RawMessage(`{"command":"ls"}`), nil)
	a.Log(context.Background(), "fs.read", nil, errors.New("boom"))
	a.Log(context.Background(), "sys.info", nil, nil)
	a.Close()
	got, err := ReadAudit(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Method != "sys.info" || got[1].Method != "fs.read" || got[1].OK || got[1].Err != "boom" {
		t.Fatalf("got %+v", got)
	}
}

func TestAuditRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	a.max = 200 // a couple of lines
	for i := range 10 {
		a.Log(context.Background(), fmt.Sprintf("m%d", i), nil, nil)
	}
	a.Close()
	cur, _ := os.Stat(path)
	old, err := os.Stat(path + ".1")
	if err != nil || cur.Size() > 300 || old.Size() > 300 {
		t.Fatalf("not rotated: cur %v old %v err %v", cur.Size(), old, err)
	}
	// Reading spans the current and the rotated file, newest first.
	got, err := ReadAudit(path, 4)
	if err != nil || len(got) != 4 || got[0].Method != "m9" || got[3].Method != "m6" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Reopening an existing log keeps counting its size.
	b, _ := NewAudit(path)
	defer b.Close()
	if b.size != cur.Size() {
		t.Fatalf("size %d, want %d", b.size, cur.Size())
	}
}

func TestAuditRedactsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := NewAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	var fed Entry
	a.Notify(func(e Entry) { fed = e })
	args := `{"recipe":{"name":"x"},"secrets":{"HF_TOKEN":"hf_abc"},"sources":[{"url":"http://p:7700/files/x/","token":"tok123"}],"headers":{"Authorization":"Bearer zzz"}}`
	a.Log(context.Background(), "recipe.apply", json.RawMessage(args), nil)
	a.Close()
	data, _ := os.ReadFile(path)
	for _, leak := range []string{"hf_abc", "tok123", "zzz"} {
		if strings.Contains(string(data), leak) || strings.Contains(fed.Args, leak) {
			t.Fatalf("%q leaked: %s / %s", leak, data, fed.Args)
		}
	}
	if !strings.Contains(fed.Args, `"name":"x"`) {
		t.Fatalf("lost the rest: %s", fed.Args)
	}
}

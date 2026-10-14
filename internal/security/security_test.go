package security

import "testing"

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

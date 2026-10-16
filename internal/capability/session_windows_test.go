//go:build windows

package capability

import (
	"strings"
	"testing"
)

func TestXMLEscape(t *testing.T) {
	got := xmlEscape(`C:\a & b <x> "q"`)
	want := `C:\a &amp; b &lt;x&gt; &quot;q&quot;`
	if got != want {
		t.Errorf("xmlEscape = %q, want %q", got, want)
	}
}

func TestUnlockArgsOnly(t *testing.T) {
	exe := `C:\ProgramData\Astrea\astrea-agent.exe`
	cmd := `"` + exe + `" unlock --desktop --result "C:\ProgramData\Astrea\unlock-result.json" --capture`
	got := unlockArgsOnly(cmd, exe)
	want := `unlock --desktop --result "C:\ProgramData\Astrea\unlock-result.json" --capture`
	if got != want {
		t.Errorf("unlockArgsOnly = %q, want %q", got, want)
	}
}

func TestUnlockTaskXML(t *testing.T) {
	exe := `C:\ProgramData\Astrea\astrea-agent.exe`
	cmd := `"` + exe + `" unlock --result "C:\ProgramData\Astrea\unlock-result.json"`
	xml := unlockTaskXML(exe, cmd)
	// SYSTEM principal and the split Command/Arguments must be present.
	for _, sub := range []string{"<UserId>S-1-5-18</UserId>", "HighestAvailable",
		"<Command>" + exe + "</Command>", "<Arguments>unlock --result"} {
		if !strings.Contains(xml, sub) {
			t.Errorf("task XML missing %q\n%s", sub, xml)
		}
	}
}

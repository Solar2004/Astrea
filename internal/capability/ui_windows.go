//go:build windows

package capability

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Solar2004/Astrea/internal/procutil"
)

func init() { ui = &winUI{} }

// winUI reads the UI Automation tree with a bundled PowerShell script. This
// avoids hand-written COM vtable calls (System.Windows.Automation does the
// walking) at the cost of ~1-2s per snapshot — fine for an occasional "read
// the window" and far more maintainable. The last snapshot is kept so ui.act
// can click an element by id.
type winUI struct {
	mu   sync.Mutex
	last map[int]UIElement
}

func (u *winUI) Center(id int) (int, int, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	e, ok := u.last[id]
	return e.X, e.Y, ok
}

func (u *winUI) Last() []UIElement {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UIElement, 0, len(u.last))
	for _, e := range u.last {
		out = append(out, e)
	}
	return out
}

// psSnapshot walks the foreground window (or one whose title contains $Window)
// and prints a JSON array of interactive elements. Coordinates are the screen
// center of each element.
const psSnapshot = `
$ErrorActionPreference='Stop'
Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes 2>$null
Add-Type @"
using System;using System.Runtime.InteropServices;
public class AstreaFg { [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow(); }
"@ 2>$null
$AE=[System.Windows.Automation.AutomationElement]
$TS=[System.Windows.Automation.TreeScope]
$want='%s'
if ($want -eq 'taskbar') {
  # The taskbar has no title: find it by class (its buttons are the open apps).
  $root=$AE::RootElement.FindFirst($TS::Children,(New-Object System.Windows.Automation.PropertyCondition($AE::ClassNameProperty,'Shell_TrayWnd')))
  if (-not $root) { '[]'; exit }
} elseif ($want -ne '') {
  $root=$null
  foreach ($w in $AE::RootElement.FindAll($TS::Children,[System.Windows.Automation.Condition]::TrueCondition)) {
    if ($w.Current.Name -and $w.Current.Name.ToLower().Contains($want.ToLower())) { $root=$w; break }
  }
  if (-not $root) { '[]'; exit }
} else {
  $h=[AstreaFg]::GetForegroundWindow()
  $root=$AE::FromHandle($h)
}
$title=$root.Current.Name
$roles=@{ 'button'='button';'edit'='edit';'checkbox'='checkbox';'radiobutton'='radio';'combobox'='combo';
  'menuitem'='menuitem';'hyperlink'='link';'listitem'='listitem';'tabitem'='tab';'treeitem'='treeitem';
  'splitbutton'='button';'menu'='menu';'document'='document';'text'='text' }
$cond=New-Object System.Windows.Automation.PropertyCondition($AE::IsControlElementProperty,$true)
$els=$root.FindAll($TS::Descendants,$cond)
$out=New-Object System.Collections.ArrayList
$n=0
foreach ($e in $els) {
  if ($n -ge 250) { break }
  try {
    $ct=$e.Current.ControlType.ProgrammaticName -replace 'ControlType\.',''
    $key=$ct.ToLower()
    if (-not $roles.ContainsKey($key)) { continue }
    $name=$e.Current.Name
    if (($key -eq 'text' -or $key -eq 'document') -and -not $name) { continue }
    $r=$e.Current.BoundingRectangle
    if ($r.Width -le 0 -or $r.Height -le 0 -or [double]::IsInfinity($r.X)) { continue }
    $val=''
    try { $vp=$e.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern); if ($vp){$val=$vp.Current.Value} } catch {}
    [void]$out.Add([pscustomobject]@{
      role=$roles[$key]; name=$name; value=$val;
      x=[int]($r.X+$r.Width/2); y=[int]($r.Y+$r.Height/2);
      w=[int]$r.Width; h=[int]$r.Height; enabled=$e.Current.IsEnabled; title=$title })
    $n++
  } catch {}
}
# Pure ASCII out (\uXXXX for the rest): through the console code page a ” in
# a title became a bare " (best-fit) and broke the JSON, and accents came out
# as garbage. Escaped, the code page cannot touch them.
$json=$out | ConvertTo-Json -Depth 3 -Compress
[regex]::Replace($json,'[^\x00-\x7F]',{param($m) '\u{0:x4}' -f [int][char]$m.Value})
`

func (u *winUI) Snapshot(window string) (string, []UIElement, error) {
	window = strings.ReplaceAll(window, "'", "''")
	script := fmt.Sprintf(psSnapshot, window)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	procutil.NoConsole(cmd) // runs in the user's desktop: no console flashing on their screen
	// UIA on a big tree can be slow; keep it under the hub's call timeout.
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		_ = cmd.Process.Kill()
		return "", nil, fmt.Errorf("ui.snapshot timed out (the window may be busy)")
	}
	if err != nil {
		return "", nil, fmt.Errorf("UI Automation: %v: %s", err, strings.TrimSpace(string(out)))
	}
	text := strings.TrimSpace(string(out))
	if text == "" || text == "[]" {
		u.store(nil)
		return "", nil, nil
	}
	// ConvertTo-Json emits an object (not an array) for a single element.
	if strings.HasPrefix(text, "{") {
		text = "[" + text + "]"
	}
	var raw []struct {
		Role    string  `json:"role"`
		Name    string  `json:"name"`
		Value   string  `json:"value"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
		W       float64 `json:"w"`
		H       float64 `json:"h"`
		Enabled bool    `json:"enabled"`
		Title   string  `json:"title"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return "", nil, fmt.Errorf("parse UI tree: %v", err)
	}
	title := ""
	els := make([]UIElement, 0, len(raw))
	for i, r := range raw {
		if r.Title != "" {
			title = r.Title
		}
		els = append(els, UIElement{
			ID: i, Role: r.Role, Name: strings.TrimSpace(r.Name), Value: r.Value,
			X: int(r.X), Y: int(r.Y), W: int(r.W), H: int(r.H), Enabled: r.Enabled,
		})
	}
	// Order roughly top-to-bottom, left-to-right so ids read naturally.
	sort.SliceStable(els, func(i, j int) bool {
		if abs(els[i].Y-els[j].Y) > 12 {
			return els[i].Y < els[j].Y
		}
		return els[i].X < els[j].X
	})
	for i := range els {
		els[i].ID = i
	}
	u.store(els)
	return title, els, nil
}

func (u *winUI) store(els []UIElement) {
	m := make(map[int]UIElement, len(els))
	for _, e := range els {
		m[e.ID] = e
	}
	u.mu.Lock()
	u.last = m
	u.mu.Unlock()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

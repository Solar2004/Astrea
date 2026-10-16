//go:build windows

package capability

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Solar2004/Astrea/internal/procutil"
	"golang.org/x/sys/windows/registry"
)

func init() {
	nolock = winNoLock{}
	unlock = winUnlock{}
}

// astreaDataDir is C:\ProgramData\Astrea: writable by both the agent (the
// elevated desktop user) and the SYSTEM helper, so they share files there.
func astreaDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Astrea")
	}
	return filepath.Join(os.TempDir(), "Astrea")
}

// ---------------------------------------------------------------------------
// session.nolock — stop the PC from locking itself (reversible)
// ---------------------------------------------------------------------------
//
// Role: this runs in the headless/full agent, which the installer registers as
// a Highest-RunLevel task for the desktop user (aluno). That user is a local
// admin, so HKLM is writable; and because they are the one logged in on the
// console, HKEY_CURRENT_USER resolves to their already-loaded hive. One
// process can therefore touch both HKLM and HKCU — no GUI helper needed.

type winNoLock struct{}

// regLever is one registry setting we flip. root is HKLM or HKCU.
type regLever struct {
	label string
	root  registry.Key
	path  string
	name  string
	// want is the value set when turning locking off.
	wantDWord uint32
	isDWord   bool
	wantSZ    string
}

func nolockLevers() []regLever {
	return []regLever{
		{label: "NoLockScreen", root: registry.LOCAL_MACHINE,
			path: `SOFTWARE\Policies\Microsoft\Windows\Personalization`, name: "NoLockScreen",
			isDWord: true, wantDWord: 1},
		{label: "InactivityTimeoutSecs", root: registry.LOCAL_MACHINE,
			path: `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, name: "InactivityTimeoutSecs",
			isDWord: true, wantDWord: 0},
		{label: "ScreenSaverIsSecure", root: registry.CURRENT_USER,
			path: `Control Panel\Desktop`, name: "ScreenSaverIsSecure",
			isDWord: false, wantSZ: "0"},
	}
}

// savedVal is a registry value captured before we changed it, so on:false can
// put things back exactly as they were.
type savedVal struct {
	Existed bool   `json:"existed"`
	IsDWord bool   `json:"is_dword"`
	DWord   uint32 `json:"dword,omitempty"`
	SZ      string `json:"sz,omitempty"`
}

type nolockState struct {
	Active bool                `json:"active"`
	Saved  map[string]savedVal `json:"saved"`
}

func nolockStatePath() string { return filepath.Join(astreaDataDir(), "nolock-state.json") }

func readNolockState() nolockState {
	var st nolockState
	if b, err := os.ReadFile(nolockStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Saved == nil {
		st.Saved = map[string]savedVal{}
	}
	return st
}

func writeNolockState(st nolockState) error {
	if err := os.MkdirAll(astreaDataDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(nolockStatePath(), b, 0o600)
}

// capture reads the lever's current value.
func (l regLever) capture() savedVal {
	k, err := registry.OpenKey(l.root, l.path, registry.QUERY_VALUE)
	if err != nil {
		return savedVal{Existed: false, IsDWord: l.isDWord}
	}
	defer k.Close()
	if l.isDWord {
		if v, _, err := k.GetIntegerValue(l.name); err == nil {
			return savedVal{Existed: true, IsDWord: true, DWord: uint32(v)}
		}
		return savedVal{Existed: false, IsDWord: true}
	}
	if v, _, err := k.GetStringValue(l.name); err == nil {
		return savedVal{Existed: true, IsDWord: false, SZ: v}
	}
	return savedVal{Existed: false, IsDWord: false}
}

// setDesired writes the "don't lock" value, creating the key if needed.
func (l regLever) setDesired() error {
	k, _, err := registry.CreateKey(l.root, l.path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if l.isDWord {
		return k.SetDWordValue(l.name, l.wantDWord)
	}
	return k.SetStringValue(l.name, l.wantSZ)
}

// restore puts the saved value back, or deletes it if it did not exist before.
func (l regLever) restore(s savedVal) error {
	k, _, err := registry.CreateKey(l.root, l.path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !s.Existed {
		err := k.DeleteValue(l.name)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	if s.IsDWord {
		return k.SetDWordValue(l.name, s.DWord)
	}
	return k.SetStringValue(l.name, s.SZ)
}

// powercfgConsoleLock sets CONSOLELOCK (require a password on wake) on both AC
// and DC of the active scheme. 0 = don't ask, 1 = ask (Windows default).
func powercfgConsoleLock(value int) error {
	for _, sub := range [][]string{
		{"/SETACVALUEINDEX", "SCHEME_CURRENT", "SUB_NONE", "CONSOLELOCK"},
		{"/SETDCVALUEINDEX", "SCHEME_CURRENT", "SUB_NONE", "CONSOLELOCK"},
	} {
		args := append(sub, fmt.Sprintf("%d", value))
		cmd := exec.Command("powercfg", args...)
		procutil.NoConsole(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("powercfg %v: %v: %s", args, err, out)
		}
	}
	cmd := exec.Command("powercfg", "/SETACTIVE", "SCHEME_CURRENT")
	procutil.NoConsole(cmd)
	_ = cmd.Run()
	return nil
}

func (winNoLock) Apply(on bool) (map[string]string, error) {
	changed := map[string]string{}
	levers := nolockLevers()
	if on {
		st := readNolockState()
		for _, l := range levers {
			// Keep the ORIGINAL saved value if we are already active, so a
			// second on:true does not record our own injected value.
			if !st.Active {
				st.Saved[l.label] = l.capture()
			}
			if err := l.setDesired(); err != nil {
				changed[l.label] = "failed: " + err.Error()
				continue
			}
			changed[l.label] = "disabled"
		}
		if err := powercfgConsoleLock(0); err != nil {
			changed["CONSOLELOCK"] = "failed: " + err.Error()
		} else {
			changed["CONSOLELOCK"] = "0 (no password on wake)"
		}
		st.Active = true
		if err := writeNolockState(st); err != nil {
			return changed, err
		}
		return changed, nil
	}

	// on:false — restore.
	st := readNolockState()
	for _, l := range levers {
		s, ok := st.Saved[l.label]
		if !ok {
			s = savedVal{Existed: false, IsDWord: l.isDWord} // unknown: delete
		}
		if err := l.restore(s); err != nil {
			changed[l.label] = "failed: " + err.Error()
			continue
		}
		if s.Existed {
			changed[l.label] = "restored"
		} else {
			changed[l.label] = "removed"
		}
	}
	if err := powercfgConsoleLock(1); err != nil {
		changed["CONSOLELOCK"] = "failed: " + err.Error()
	} else {
		changed["CONSOLELOCK"] = "1 (password on wake, default)"
	}
	st.Active = false
	st.Saved = map[string]savedVal{}
	_ = writeNolockState(st)
	return changed, nil
}

func (winNoLock) State() (map[string]any, error) {
	st := readNolockState()
	now := map[string]any{}
	for _, l := range nolockLevers() {
		s := l.capture()
		if !s.Existed {
			now[l.label] = nil
		} else if s.IsDWord {
			now[l.label] = s.DWord
		} else {
			now[l.label] = s.SZ
		}
	}
	return map[string]any{"active": st.Active, "values": now}, nil
}

// ---------------------------------------------------------------------------
// session.unlock — sign in from the lock screen via a SYSTEM helper
// ---------------------------------------------------------------------------
//
// The agent runs as the desktop user (admin), not as SYSTEM, so it cannot
// reach the Winlogon secure desktop. Like RustDesk, we register a one-shot
// scheduled task that runs `astrea-agent unlock` as SYSTEM; that process hops
// into the console session's Winlogon desktop and presses the keys to sign in
// (see cmd/astrea-agent/unlock_windows.go). It writes the outcome to a shared
// file we read back here, then we delete the task.

type winUnlock struct{}

const unlockTaskName = "AstreaUnlock"

func unlockResultPath() string { return filepath.Join(astreaDataDir(), "unlock-result.json") }

func (winUnlock) Unlock(capture bool) (map[string]any, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(astreaDataDir(), 0o755); err != nil {
		return nil, err
	}
	resPath := unlockResultPath()
	_ = os.Remove(resPath)

	args := fmt.Sprintf(`"%s" unlock --result "%s"`, exe, resPath)
	if capture {
		args += " --capture"
	}
	xmlPath := filepath.Join(astreaDataDir(), "unlock-task.xml")
	if err := writeUTF16LE(xmlPath, unlockTaskXML(exe, args)); err != nil {
		return nil, err
	}
	defer os.Remove(xmlPath)
	defer runSchtasks("/delete", "/tn", unlockTaskName, "/f")

	if out, err := runSchtasks("/create", "/tn", unlockTaskName, "/xml", xmlPath, "/f"); err != nil {
		return nil, fmt.Errorf("schtasks create: %v: %s", err, out)
	}
	if out, err := runSchtasks("/run", "/tn", unlockTaskName); err != nil {
		return nil, fmt.Errorf("schtasks run: %v: %s", err, out)
	}

	// The SYSTEM task and its desktop child write the result; poll for it.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(resPath); err == nil && len(b) > 0 {
			var res map[string]any
			if err := json.Unmarshal(b, &res); err != nil {
				return nil, fmt.Errorf("bad result file: %v", err)
			}
			_ = os.Remove(resPath)
			return res, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("the SYSTEM unlock helper did not report back in time (see C:\\ProgramData\\Astrea\\agent.log)")
}

func runSchtasks(args ...string) (string, error) {
	cmd := exec.Command("schtasks", args...)
	procutil.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// unlockTaskXML builds a Task Scheduler definition that runs command as SYSTEM
// with the highest privileges, once, on demand (we trigger it with schtasks
// /run). command is the full command line; the exe is also given separately.
func unlockTaskXML(exe, command string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Astrea: sign in from the lock screen (one-shot).</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT2M</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(exe) + `</Command>
      <Arguments>` + xmlEscape(unlockArgsOnly(command, exe)) + `</Arguments>
    </Exec>
  </Actions>
</Task>`
}

// unlockArgsOnly strips the leading "exe" token from command, leaving just the
// arguments for the XML <Arguments> element.
func unlockArgsOnly(command, exe string) string {
	return strings.TrimPrefix(command, `"`+exe+`" `)
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// writeUTF16LE writes s as UTF-16 little-endian with a BOM, the encoding
// schtasks /xml expects.
func writeUTF16LE(path, s string) error {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(u)*2+2)
	buf = append(buf, 0xFF, 0xFE)
	for _, r := range u {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return os.WriteFile(path, buf, 0o600)
}

//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Solar2004/Astrea/internal/guibridge"
	"github.com/Solar2004/Astrea/internal/procutil"
)

var procWTSQuerySessionInformation = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

const wtsUserName = 5 // WTS_INFO_CLASS WTSUserName

// consoleUser names the user signed in at the physical console, "" if nobody
// (sign-in screen, or no console session at all).
func consoleUser() string {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xFFFFFFFF {
		return ""
	}
	var buf *uint16
	var n uint32
	r, _, _ := procWTSQuerySessionInformation.Call(0, uintptr(id), wtsUserName,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == nil {
		return ""
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf)))
	return windows.UTF16PtrToString(buf)
}

// adminsSID is BUILTIN\Administrators, whatever the Windows language calls it.
const adminsSID = "S-1-5-32-544"

// consoleUserIsAdmin reports whether the user signed in at the console belongs
// to the Administrators group. It checks the console session's token and, for
// a UAC-split administrator whose interactive token carries Administrators only
// as deny-only, its linked (elevated) token. A standard account has the group
// in neither, so it returns false — the signal to launch the pupil helper.
func consoleUserIsAdmin() (bool, error) {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xFFFFFFFF {
		return false, fmt.Errorf("no active console session")
	}
	var tok windows.Token
	if err := windows.WTSQueryUserToken(id, &tok); err != nil {
		return false, fmt.Errorf("WTSQueryUserToken(session %d): %w", id, err)
	}
	defer tok.Close()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	if tokenHasEnabled(tok, admins) {
		return true, nil
	}
	if linked, err := tok.GetLinkedToken(); err == nil {
		defer linked.Close()
		if tokenHasEnabled(linked, admins) {
			return true, nil
		}
	}
	return false, nil
}

// tokenHasEnabled reports whether tok carries sid as an enabled group (not a
// deny-only one, which is how UAC keeps a filtered admin from using it).
func tokenHasEnabled(tok windows.Token, sid *windows.SID) bool {
	groups, err := tok.GetTokenGroups()
	if err != nil {
		return false
	}
	for _, g := range groups.AllGroups() {
		if g.Sid.Equals(sid) &&
			g.Attributes&windows.SE_GROUP_ENABLED != 0 &&
			g.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			return true
		}
	}
	return false
}

// migrateGUITask re-registers the gui helper task for any administrator who
// signs in. Installs up to v0.6.4 tied it to the installing account, so when
// another admin signed in (f1-6, "admin" instead of Aluno(a), 2026-10-26) the
// helper never started in that session and relaunching the task could not
// fix it. It reports whether it changed anything; a PC without the task
// (classic install) is left alone.
func migrateGUITask(cfgPath, guiAddr string) (bool, error) {
	out, err := runSchtasks("/query", "/tn", guiTask, "/xml")
	if err != nil {
		return false, nil
	}
	cur := strings.ReplaceAll(out, "\x00", "") // UTF-16 or not, depending on the console
	if strings.Contains(cur, "<GroupId>"+adminsSID+"</GroupId>") && strings.Contains(cur, "<MultipleInstancesPolicy>Parallel<") {
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	dir := filepath.Dir(exe)
	xmlPath := filepath.Join(dir, "gui-task.xml")
	args := fmt.Sprintf(`--config "%s" --role gui --gui-addr %s`, cfgPath, guiAddr)
	if err := writeUTF16LE(xmlPath, guiTaskXML(exe, args, dir)); err != nil {
		return false, err
	}
	defer os.Remove(xmlPath)
	if out, err := runSchtasks("/create", "/tn", guiTask, "/xml", xmlPath, "/f"); err != nil {
		return false, fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return true, nil
}

// guiTaskXML is the gui helper task: at the logon of any user, run as that
// user, elevated, for members of Administrators (the helper reads the token in
// config.json, which only admins can). Parallel, so a second admin signing in
// gets a helper too; extra copies give way by themselves (listenRetry,
// yieldOffConsole).
func guiTaskXML(exe, args, dir string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.3" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <URI>\` + guiTask + `</URI>
    <Description>Astrea: screen, mouse and keyboard helper for whoever signs in.</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <GroupId>` + adminsSID + `</GroupId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>
    <RestartOnFailure>
      <Count>999</Count>
      <Interval>PT1M</Interval>
    </RestartOnFailure>
    <StartWhenAvailable>true</StartWhenAvailable>
  </Settings>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(exe) + `</Command>
      <Arguments>` + esc(args) + `</Arguments>
      <WorkingDirectory>` + esc(dir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

func runSchtasks(args ...string) (string, error) {
	cmd := exec.Command("schtasks", args...)
	procutil.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeUTF16LE writes s the way schtasks /xml wants it.
func writeUTF16LE(path, s string) error {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(u)*2+2)
	buf = append(buf, 0xFF, 0xFE)
	for _, r := range u {
		buf = append(buf, byte(r), byte(r>>8))
	}
	return os.WriteFile(path, buf, 0o600)
}

// offConsole reports whether this process runs in a session other than the
// console's while someone is signed in at the console: a gui helper left
// behind in a disconnected session after a user switch, holding the port the
// console user's helper needs.
func offConsole() bool {
	console := windows.WTSGetActiveConsoleSessionId()
	if console == 0xFFFFFFFF {
		return false
	}
	var own uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &own) != nil || own == console {
		return false
	}
	return consoleUser() != ""
}

// launchHelper brings the gui helper back for whoever is signed in at the
// console, picking the mechanism by whether that user is an administrator.
//
//   - Administrator: the logon task (startGUITask). It runs elevated as that
//     user and reads the token from config.json, so the bridge talks to it
//     with cfg.Token.
//   - Standard account (a pupil): the headless agent (SYSTEM) launches the
//     helper itself in the console session, as that user but with administrator
//     rights injected into the process token only (launchConsoleHelper; the SAM
//     account is never touched). It authenticates with a per-launch secret the
//     helper reads from its environment, so the bridge switches to that secret.
//
// It returns a short description for the log and whether the launch failed.
func launchHelper(b *guibridge.Bridge, cfgToken, guiAddr string, logger *log.Logger) (string, error) {
	enablePrivileges(logger) // WTSQueryUserToken and the S4U logon below need SeTcbPrivilege
	admin, err := consoleUserIsAdmin()
	if err != nil {
		// Could not tell (no console token, etc.): keep the pre-existing, safe
		// behaviour and use the logon task, which only ever runs for admins.
		logger.Printf("gui helper: could not tell whether %s is an administrator (%v); using the logon task", consoleUser(), err)
		admin = true
	}
	if admin {
		b.SetToken(cfgToken)
		return "task " + guiTask, startGUITask()
	}
	secret, err := newGUISecret()
	if err != nil {
		return "", err
	}
	user := consoleUser()
	if err := launchConsoleHelper(secret, guiAddr, logger); err != nil {
		return "", err
	}
	// The helper authenticates with the secret, never with cfg.Token: a pupil
	// must not end up holding the token that drives the SYSTEM agent.
	b.SetToken(secret)
	logger.Printf("gui helper: launched helper as %s (not an administrator)", user)
	return fmt.Sprintf("helper as %s (not an administrator)", user), nil
}

// newGUISecret is the per-launch bearer for a standard-user helper: 32 random
// bytes, hex. It replaces cfg.Token on that helper's loopback server.
func newGUISecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// startGUITask runs the logon task that starts the gui helper in the
// signed-in user's session.
func startGUITask() error {
	if out, err := runSchtasks("/run", "/tn", guiTask); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// killSessionHelpers terminates every copy of this binary running in the
// console session (the gui helpers, hung or duplicated), never this process
// and never a copy started from another folder. It returns how many it
// killed.
func killSessionHelpers() (int, error) {
	console := windows.WTSGetActiveConsoleSessionId()
	if console == 0xFFFFFFFF {
		return 0, nil
	}
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	exe := strings.ToLower(filepath.Base(self))
	dir := filepath.Dir(self)
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	killed := 0
	var errs []string
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID == uint32(os.Getpid()) || strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])) != exe {
			continue
		}
		var sess uint32
		if windows.ProcessIdToSessionId(pe.ProcessID, &sess) != nil || sess != console {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			errs = append(errs, fmt.Sprintf("pid %d: %v", pe.ProcessID, err))
			continue
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil ||
			!strings.EqualFold(filepath.Dir(windows.UTF16ToString(buf[:n])), dir) {
			windows.CloseHandle(h)
			continue
		}
		if err := windows.TerminateProcess(h, 1); err != nil {
			errs = append(errs, fmt.Sprintf("pid %d: %v", pe.ProcessID, err))
		} else {
			killed++
		}
		windows.CloseHandle(h)
	}
	if len(errs) > 0 {
		return killed, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return killed, nil
}

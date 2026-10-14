//go:build windows

package deps

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Solar2004/Astrea/internal/procutil"
	"golang.org/x/sys/windows/registry"
)

// lookPath is exec.LookPath minus the Microsoft Store "app execution alias"
// stubs: on a fresh Windows, python.exe in WindowsApps exists but only opens
// the Store, so it must not count as Python being installed.
func lookPath(name string) (string, error) {
	if strings.EqualFold(name, "winget") {
		if p := realWinget(); p != "" {
			return p, nil
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if isStoreStub(name, p) {
		return "", errors.New("only the Microsoft Store alias is present")
	}
	return p, nil
}

func isStoreStub(name, path string) bool {
	switch strings.ToLower(name) {
	case "python", "python3":
	default:
		return false
	}
	return strings.Contains(strings.ToLower(path), `\microsoft\windowsapps\`)
}

// RefreshPath rebuilds this process's PATH from the registry (machine + user)
// after an install, the way a new logon would. Without it, anything winget
// just installed stays invisible to the agent and to every command it runs
// until the agent restarts. Entries already in the current PATH are kept.
func RefreshPath() {
	var parts []string
	parts = append(parts, regPath(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`)...)
	parts = append(parts, regPath(registry.CURRENT_USER, `Environment`)...)
	// winget puts portable packages' shims here; it is on the user PATH only
	// after a new logon.
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		parts = append(parts, filepath.Join(la, "Microsoft", "WinGet", "Links"))
	}
	parts = append(parts, filepath.SplitList(os.Getenv("PATH"))...)

	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		k := strings.ToLower(strings.TrimRight(p, `\`))
		if p == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	_ = os.Setenv("PATH", strings.Join(out, string(os.PathListSeparator)))
}

func regPath(root registry.Key, key string) []string {
	k, err := registry.OpenKey(root, key, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if err != nil {
		return nil
	}
	if exp, err := registry.ExpandString(v); err == nil {
		v = exp
	}
	return filepath.SplitList(v)
}

var (
	wingetOnce sync.Once
	wingetPath string
)

// realWinget returns winget.exe inside the App Installer package. The usual
// "winget" on PATH is an app execution alias (%LOCALAPPDATA%\Microsoft\
// WindowsApps), which tends to fail outside an interactive session — exactly
// where the headless agent runs after a reboot. The real binary works from
// any session as the same user.
func realWinget() string {
	wingetOnce.Do(func() {
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-AppxPackage -Name Microsoft.DesktopAppInstaller | Sort-Object Version -Descending | Select-Object -First 1).InstallLocation")
		procutil.NoConsole(cmd)
		out, err := cmd.Output()
		if err != nil {
			return
		}
		p := filepath.Join(strings.TrimSpace(string(out)), "winget.exe")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			wingetPath = p
		}
	})
	return wingetPath
}

//go:build windows

package deps

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// lookPath is exec.LookPath minus the Microsoft Store "app execution alias"
// stubs: on a fresh Windows, python.exe in WindowsApps exists but only opens
// the Store, so it must not count as Python being installed.
func lookPath(name string) (string, error) {
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

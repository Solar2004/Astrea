//go:build windows

package capability

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// installedPrograms lists the programs in "Apps & features" (the Uninstall
// registry keys), without system components and updates.
func installedPrograms() []string {
	var names []string
	roots := []struct {
		key  registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}
	for _, r := range roots {
		k, err := registry.OpenKey(r.key, r.path, registry.ENUMERATE_SUB_KEYS|registry.READ)
		if err != nil {
			continue
		}
		subs, _ := k.ReadSubKeyNames(-1)
		for _, s := range subs {
			sk, err := registry.OpenKey(k, s, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			name, _, _ := sk.GetStringValue("DisplayName")
			sys, _, _ := sk.GetIntegerValue("SystemComponent")
			parent, _, _ := sk.GetStringValue("ParentKeyName")
			sk.Close()
			name = strings.TrimSpace(name)
			if name == "" || sys == 1 || parent != "" || strings.HasPrefix(name, "Update for") ||
				strings.HasPrefix(name, "Security Update") || strings.HasPrefix(name, "Microsoft Visual C++") {
				continue
			}
			names = append(names, name)
		}
		k.Close()
	}
	return sortedUnique(names)
}

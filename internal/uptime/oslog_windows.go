//go:build windows

package uptime

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Kernel-Power 41/42/107, EventLog 6005/6006/6008 and Power-Troubleshooter 1:
// boots, clean shutdowns, sleeps, wakes and crashes.
const psEvents = `$ErrorActionPreference='SilentlyContinue'
Get-WinEvent -FilterHashtable @{LogName='System';Id=@(1,41,42,107,6005,6006,6008);StartTime=(Get-Date).AddDays(-%d)} |
 Where-Object { $_.Id -ne 1 -or $_.ProviderName -eq 'Microsoft-Windows-Power-Troubleshooter' } |
 ForEach-Object { '{0} {1}' -f $_.Id, $_.TimeCreated.ToUniversalTime().ToString('o') }`

func powershell(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	return string(out), err
}

func osSpans(ctx context.Context, since time.Time) []Span {
	days := int(time.Since(since).Hours()/24) + 1
	out, err := powershell(ctx, fmt.Sprintf(psEvents, days))
	if err != nil && out == "" {
		return nil
	}
	return spansFromEvents(parseWindowsEvents(out))
}

// endKindAfter tells how the boot that was alive at t ended: a crash leaves
// Kernel-Power 41 or EventLog 6008 at the next boot.
func endKindAfter(t time.Time) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue'
(Get-WinEvent -FilterHashtable @{LogName='System';Id=@(41,6008);StartTime=[datetime]::Parse('%s').ToLocalTime()} -MaxEvents 1 | Measure-Object).Count`, t.UTC().Format(time.RFC3339))
	if out, _ := powershell(ctx, script); len(out) > 0 && out[0] != '0' {
		return EndCrash
	}
	return EndOff
}

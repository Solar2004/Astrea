package capability

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/Solar2004/Astrea/internal/procutil"
	"github.com/Solar2004/Astrea/internal/protocol"
)

// --- power.standby ---

// PowerStandby reports why a Windows PC keeps dropping off the network and,
// as a last resort, turns Modern Standby off.
//
// On Modern Standby PCs (S0 low-power idle, most recent hardware) the screen
// turning off after its idle timeout puts the whole PC in standby: desktop
// processes are frozen and services throttled, so the agent and remote tools
// like RustDesk answer only now and then. The agent's first defence is a power
// request (power.awake always). Disabling Modern Standby (PlatformAoAcOverride=0)
// is the fallback: with no S3 in the firmware the PC then simply never sleeps,
// while the screen still turns off. It takes effect after a reboot.
type PowerStandby struct{}

func (PowerStandby) Name() string { return "power.standby" }

func (PowerStandby) Describe() Descriptor {
	return Descriptor{
		Name: "power.standby",
		Description: "Windows standby diagnosis. Without arguments: whether the PC uses Modern Standby (the screen turning off " +
			"puts it to sleep, freezing the agent and remote tools), how many times it entered standby today and why, the " +
			"screen-off timeout, and the active power requests (powercfg /requests: Astrea's shows under SYSTEM and EXECUTION " +
			"while power.awake holds it). modern_standby=false disables Modern Standby (registry PlatformAoAcOverride=0) so " +
			"the PC never sleeps; true restores the default. Either change needs a reboot. Try power.awake mode=always first.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"modern_standby": {"type": "boolean", "description": "false: disable Modern Standby. true: restore. Omit: read state."}}
		}`),
	}
}

const powerKey = `HKLM\SYSTEM\CurrentControlSet\Control\Power`

// standbyScript prints key=value lines; standby events are "506|time|reason".
const standbyScript = `$ErrorActionPreference='SilentlyContinue'
$a = powercfg /a | Out-String
$i = $a.IndexOf('S0 Low Power Idle'); $n = $a.IndexOf('not available')
"modern=" + ($i -ge 0 -and ($n -lt 0 -or $i -lt $n))
"override=" + (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\Power').PlatformAoAcOverride
$v = (powercfg /q SCHEME_CURRENT SUB_VIDEO VIDEOIDLE | Select-String 'AC Power Setting Index').ToString().Split(':')[1].Trim()
"screen_off_sec=" + [Convert]::ToInt32($v, 16)
$ev = @(Get-WinEvent -FilterHashtable @{LogName='System';Id=506,507;StartTime=(Get-Date).Date})
"standby_today=" + @($ev | ? Id -eq 506).Count
$ev | Select -First 6 | % { "event=" + $_.Id + "|" + $_.TimeCreated.ToUniversalTime().ToString('o') + "|" + (($_.Message -split ':')[-1].Trim()) }
"requests=" + ((powercfg /requests | Out-String) -replace "\r?\n", "\n")`

func (PowerStandby) Invoke(ctx context.Context, payload json.RawMessage, _ Emit) (any, error) {
	if runtime.GOOS != "windows" {
		return nil, unsupported("power.standby")
	}
	var req struct {
		ModernStandby *bool `json:"modern_standby"`
	}
	if len(payload) > 0 && json.Unmarshal(payload, &req) != nil {
		return nil, &protocol.Error{Code: protocol.CodeBadRequest, Message: "invalid arguments"}
	}
	if req.ModernStandby != nil {
		args := []string{"delete", powerKey, "/v", "PlatformAoAcOverride", "/f"}
		if !*req.ModernStandby {
			args = []string{"add", powerKey, "/v", "PlatformAoAcOverride", "/t", "REG_DWORD", "/d", "0", "/f"}
		}
		cmd := exec.CommandContext(ctx, "reg", args...)
		procutil.NoConsole(cmd)
		// Restoring when the value is not there fails, and that is fine.
		if err := cmd.Run(); err != nil && !*req.ModernStandby {
			return nil, internal(err)
		}
	}
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", standbyScript)
	procutil.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, internal(err)
	}
	st := map[string]any{"standby_events": []map[string]string{}}
	var events []map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "modern":
			st["modern_standby"] = v == "True"
		case "override":
			st["modern_standby_disabled"] = v == "0" // PlatformAoAcOverride=0
		case "screen_off_sec", "standby_today":
			st[k], _ = strconv.Atoi(v)
		case "event":
			if f := strings.SplitN(v, "|", 3); len(f) == 3 {
				kind := "enter"
				if f[0] == "507" {
					kind = "exit"
				}
				events = append(events, map[string]string{"kind": kind, "time": f[1], "reason": f[2]})
			}
		case "requests":
			st["power_requests"] = strings.ReplaceAll(v, `\n`, "\n")
		}
	}
	if events != nil {
		st["standby_events"] = events
	}
	if req.ModernStandby != nil {
		st["reboot_required"] = true
	}
	return st, nil
}

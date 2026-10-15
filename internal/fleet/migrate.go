package fleet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Solar2004/Astrea/internal/update"
)

// Agents from v0.2.x cannot update themselves (no agent.update), but they do
// have exec. Migrate uses it to put a current agent in place once; from then
// on the agent keeps itself up to date.
//
// Windows: the script downloads the new exe, checks it against the release's
// SHA256SUMS.txt, renames the running exe aside (allowed while it runs) and
// hands the restart to a one-shot scheduled task, so stopping the agent does
// not kill the script that restarts it.
// Linux: same swap, then systemd restarts the user unit.

const migrateWindows = `$ErrorActionPreference = 'Stop'
$base = '{{BASE}}/download/{{TAG}}'
$t = Get-ScheduledTask -TaskName 'AstreaAgent'
$exe = $t.Actions[0].Execute.Trim('"')
$new = "$exe.new"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$ProgressPreference = 'SilentlyContinue'
Invoke-WebRequest "$base/astrea-agent.exe" -OutFile $new -UseBasicParsing
$sums = [Text.Encoding]::UTF8.GetString((Invoke-WebRequest "$base/SHA256SUMS.txt" -UseBasicParsing).RawContentStream.ToArray())
$want = ($sums -split "\r?\n" | Where-Object { $_ -match '^([0-9a-fA-F]{64})\s+\*?astrea-agent\.exe\s*$' } | ForEach-Object { $Matches[1].ToLower() } | Select-Object -First 1)
$got = (Get-FileHash $new -Algorithm SHA256).Hash.ToLower()
if (-not $want -or $got -ne $want) { Remove-Item $new -Force; throw "checksum no coincide ($got vs $want)" }
Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
Move-Item $exe "$exe.old" -Force
Move-Item $new $exe -Force
$restart = @'
$exe = '__EXE__'
function Restart-Agent {
  foreach ($n in 'AstreaAgent', 'AstreaAgentGUI') {
    if (Get-ScheduledTask -TaskName $n -ErrorAction SilentlyContinue) {
      Stop-ScheduledTask -TaskName $n -ErrorAction SilentlyContinue
      Start-Sleep -Seconds 1
      Start-ScheduledTask -TaskName $n -ErrorAction SilentlyContinue
    }
  }
}
Start-Sleep -Seconds 3
Restart-Agent
# Rollback: if Windows refuses the new binary (Smart App Control, WDAC,
# antivirus) the agent would never come back; put the old one back instead.
Start-Sleep -Seconds 20
if ((Get-ScheduledTask -TaskName 'AstreaAgent').State -ne 'Running' -and (Test-Path "$exe.old")) {
  Move-Item $exe "$exe.failed" -Force -ErrorAction SilentlyContinue
  Move-Item "$exe.old" $exe -Force
  Restart-Agent
}
Unregister-ScheduledTask -TaskName 'AstreaMigrate' -Confirm:$false -ErrorAction SilentlyContinue
'@
$restart = $restart.Replace('__EXE__', $exe.Replace("'", "''"))
$enc = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($restart))
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -EncodedCommand $enc"
Register-ScheduledTask -TaskName 'AstreaMigrate' -Action $action -Principal $t.Principal -Force | Out-Null
Start-ScheduledTask -TaskName 'AstreaMigrate'
Write-Output "ok: $exe -> {{TAG}}, reiniciando"
`

const migrateLinux = `set -e
exe=$(readlink -f /proc/$PPID/exe)
case "$(uname -m)" in x86_64|amd64) a=amd64;; aarch64|arm64) a=arm64;; *) echo "arquitectura no soportada"; exit 1;; esac
base='{{BASE}}/download/{{TAG}}'
curl -fsSL "$base/astrea-agent-linux-$a" -o "$exe.new"
want=$(curl -fsSL "$base/SHA256SUMS.txt" | awk -v f="astrea-agent-linux-$a" '$2==f||$2=="*"f {print $1}')
got=$(sha256sum "$exe.new" | cut -d' ' -f1)
[ -n "$want" ] && [ "$got" = "$want" ] || { rm -f "$exe.new"; echo "checksum no coincide"; exit 1; }
chmod +x "$exe.new"
mv -f "$exe.new" "$exe"
unit=$(sed -n 's#.*/\([^/]*\.service\)$#\1#p' /proc/$PPID/cgroup | tail -n1)
if [ -n "$unit" ] && systemctl --user is-active --quiet "$unit" 2>/dev/null; then
  nohup sh -c "sleep 3; systemctl --user restart --no-block '$unit'" >/dev/null 2>&1 &
  echo "ok: $exe -> {{TAG}}, reiniciando $unit"
else
  echo "ok: $exe -> {{TAG}}; no corre como servicio systemd: reinícialo a mano"
fi
`

// Migrate brings a pre-v0.3.0 agent to tag through exec.
func (n *Node) Migrate(ctx context.Context, tag string) UpdateResult {
	r := UpdateResult{Name: n.Name, Target: tag}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var admin struct {
		OS      string `json:"os"`
		Version string `json:"version"`
	}
	if err := n.Call(ctx, "admin.status", &admin); err != nil {
		r.Error = ErrOffline
		return r
	}
	if admin.Version != "" {
		r.Current = admin.Version
		r.Note = "ya tiene auto-update; usa update normal"
		return r
	}
	script, shell := migrateLinux, "bash"
	if admin.OS == "windows" {
		script, shell = migrateWindows, "powershell"
	}
	script = strings.NewReplacer("{{BASE}}", update.Base(), "{{TAG}}", tag).Replace(script)
	if shell == "powershell" {
		// v0.2.x passes the command to powershell -Command as one argument;
		// quotes and here-strings survive that only if they never appear in it.
		b64 := base64.StdEncoding.EncodeToString([]byte(script))
		script = "$s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" + b64 + "')); Invoke-Expression $s"
	}

	res, err := n.Client.Call(ctx, "exec", map[string]any{"command": script, "shell": shell, "timeout_sec": 240})
	if err != nil {
		// The restart can beat the reply; what counts is the agent coming back new.
		if v := n.waitVersion(ctx, 45*time.Second); v != "" {
			r.Current, r.Updated, r.Note = "v0.2.x", true, "migrado; ya responde "+v
			return r
		}
		r.Error = "exec: " + err.Error()
		return r
	}
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	_ = json.Unmarshal(res.Result, &out)
	text := strings.TrimSpace(out.Stdout + "\n" + out.Stderr)
	for _, ch := range res.Streams {
		text += ch.Data
	}
	text = strings.TrimSpace(text)
	if out.ExitCode != 0 || !strings.Contains(text, "ok:") {
		r.Error = fmt.Sprintf("la migración falló (código %d): %s", out.ExitCode, lastLines(text, 6))
		return r
	}
	if v := n.waitVersion(ctx, 45*time.Second); v != "" {
		r.Current, r.Updated, r.Note = "v0.2.x", true, "migrado; ya responde "+v
		return r
	}
	r.Error = "binario sustituido pero el agente no ha vuelto en 45 s: " + lastLines(text, 1)
	return r
}

// waitVersion polls admin.status until the agent reports a version (only
// v0.3.0+ do) or the wait ends. Returns "" on timeout.
func (n *Node) waitVersion(ctx context.Context, wait time.Duration) string {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(2 * time.Second):
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var st struct {
			Version string `json:"version"`
		}
		err := n.Call(cctx, "admin.status", &st)
		cancel()
		if err == nil && st.Version != "" {
			return st.Version
		}
	}
	return ""
}

// MigrateAll migrates every pre-v0.3.0 agent and leaves the rest alone.
func (f *Fleet) MigrateAll(ctx context.Context, tag string) []UpdateResult {
	out := make([]UpdateResult, len(f.Nodes))
	idx := map[*Node]int{}
	for i, n := range f.Nodes {
		idx[n] = i
	}
	f.Each(func(n *Node) { out[idx[n]] = n.Migrate(ctx, tag) })
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

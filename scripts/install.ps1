<#
.SYNOPSIS
  Instalador de Astrea para Windows. Deja la PC lista para ser controlada por
  tu agente (Hermes) sobre tu tailnet.

.DESCRIPTION
  Pensado para funcionar en un Windows recien instalado:
  - Pide permisos de administrador una sola vez (UAC) y sigue en esa ventana.
  - Instala Tailscale si falta (MSI oficial, sin depender de winget) y te
    guia para iniciar sesion.
  - Descarga el agente del ultimo release y verifica su SHA256.
  - Escribe la config (UTF-8 sin BOM) conservando el token si ya existia.
  - Abre el puerto en el Firewall de Windows solo para el tailnet.
  - Registra dos tareas programadas:
      AstreaAgent     al encender la PC, sin que nadie inicie sesion (modo
                      headless, como tu usuario): comandos, servicios (modelos),
                      descargas. Tras un reinicio los modelos vuelven solos.
      AstreaAgentGUI  al iniciar sesion: pantalla, mouse y teclado.
    Con ASTREA_HEADLESS=0 se usa el modo clasico (una sola tarea al iniciar sesion).
  - Comprueba que el agente responde y te imprime la config del hub.

.EXAMPLE
  irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1 | iex

.NOTES
  Variables de entorno opcionales antes de ejecutar:
    $env:ASTREA_TOKEN       -> usa este token (por defecto: el existente o uno nuevo)
    $env:ASTREA_PORT        -> puerto (default 7700)
    $env:ASTREA_REPO        -> owner/repo (default Solar2004/Astrea)
    $env:ASTREA_TS_AUTHKEY  -> auth key de Tailscale para entrar sin navegador
    $env:ASTREA_HEADLESS=0  -> sin modo headless: el agente solo corre con la sesion iniciada
    $env:ASTREA_GUI_PORT    -> puerto local (127.0.0.1) del ayudante GUI (default 7701)
    $env:ASTREA_UNINSTALL=1 -> desinstala el agente
#>

& {
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # la barra de progreso de PS 5.1 hace las descargas 10x mas lentas

$Repo       = if ($env:ASTREA_REPO) { $env:ASTREA_REPO } else { 'Solar2004/Astrea' }
$Port       = if ($env:ASTREA_PORT) { $env:ASTREA_PORT } else { '7700' }
$ScriptUrl  = "https://raw.githubusercontent.com/$Repo/main/scripts/install.ps1"
$ReleaseUrl = "https://github.com/$Repo/releases/latest/download"
$InstallDir = Join-Path $env:ProgramData 'Astrea'
$ExePath    = Join-Path $InstallDir 'astrea-agent.exe'
$CfgPath    = Join-Path $InstallDir 'config.json'
$LogPath    = Join-Path $InstallDir 'agent.log'
$HubCfgPath = Join-Path $InstallDir 'hub-config.txt'
$TaskName   = 'AstreaAgent'
$GuiTask    = 'AstreaAgentGUI'
$GuiPort    = if ($env:ASTREA_GUI_PORT) { $env:ASTREA_GUI_PORT } else { '7701' }
$Headless   = $env:ASTREA_HEADLESS -ne '0'
$RuleName   = 'Astrea Agent'
$Elevated   = $env:ASTREA_ELEVATED -eq '1'

function Step($msg) { Write-Host ""; Write-Host "== $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "   OK  $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "   !!  $msg" -ForegroundColor Yellow }
function Fail($msg) { throw $msg }

# Ejecuta un programa nativo sin que su stderr se convierta en excepcion
# (en Windows PowerShell 5.1, '2>$null' + ErrorActionPreference=Stop lanza).
function Invoke-Native {
  param([string]$File, [string[]]$Arguments)
  $old = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try {
    $out = & $File @Arguments 2>$null
    return [pscustomobject]@{ Code = $LASTEXITCODE; Output = @($out) }
  } finally { $ErrorActionPreference = $old }
}

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  return ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-WebFile([string]$Url, [string]$OutFile) {
  for ($i = 1; $i -le 3; $i++) {
    try {
      Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing -TimeoutSec 300
      return
    } catch {
      if ($i -eq 3) { throw "No se pudo descargar $Url : $($_.Exception.Message)" }
      Start-Sleep -Seconds (2 * $i)
    }
  }
}

# ---------------------------------------------------------------------------
# 0. Requisitos basicos
# ---------------------------------------------------------------------------
function Assert-Prerequisites {
  if ($PSVersionTable.PSVersion.Major -lt 5) {
    Fail "Se necesita Windows PowerShell 5.1 o superior (tienes $($PSVersionTable.PSVersion)). Instala WMF 5.1: https://aka.ms/wmf51download"
  }
  if (-not [Environment]::Is64BitOperatingSystem) {
    Fail "Astrea necesita Windows de 64 bits."
  }
  # GitHub y Tailscale exigen TLS 1.2; en Windows sin actualizar puede no estar activo.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}

# ---------------------------------------------------------------------------
# 1. Elevacion: relanza este mismo script como administrador
# ---------------------------------------------------------------------------
function Start-Elevated {
  Step "Se necesitan permisos de administrador"
  Write-Host "   Acepta el aviso de Windows (UAC) que va a aparecer."

  # Texto del script: el fichero local si lo hay, si no, se descarga de nuevo.
  $src = $null
  foreach ($p in @($env:ASTREA_SCRIPT, $PSCommandPath)) {
    if ($p -and (Test-Path $p)) { $src = [IO.File]::ReadAllText($p, [Text.Encoding]::UTF8); break }
  }
  if (-not $src) {
    $src = (Invoke-WebRequest -Uri $ScriptUrl -UseBasicParsing).Content
  }
  $tmp = Join-Path $env:TEMP 'astrea-install.ps1'
  [IO.File]::WriteAllText($tmp, $src, (New-Object Text.UTF8Encoding $false))

  # El proceso elevado no hereda las variables de entorno: se pasan explicitas.
  $vars = [ordered]@{
    ASTREA_ELEVATED   = '1'
    ASTREA_USER       = "$env:USERDOMAIN\$env:USERNAME"
    ASTREA_TOKEN      = $env:ASTREA_TOKEN
    ASTREA_PORT       = $env:ASTREA_PORT
    ASTREA_REPO       = $env:ASTREA_REPO
    ASTREA_TS_AUTHKEY = $env:ASTREA_TS_AUTHKEY
    ASTREA_UNINSTALL  = $env:ASTREA_UNINSTALL
    ASTREA_HEADLESS   = $env:ASTREA_HEADLESS
    ASTREA_GUI_PORT   = $env:ASTREA_GUI_PORT
  }
  $set = ($vars.GetEnumerator() | Where-Object { $_.Value } | ForEach-Object {
    "`$env:$($_.Key)='$($_.Value -replace "'", "''")'"
  }) -join '; '
  $tmpQ = $tmp -replace "'", "''"
  $cmd = "$set; Invoke-Expression ([IO.File]::ReadAllText('$tmpQ', [Text.Encoding]::UTF8))"
  $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($cmd))

  try {
    $p = Start-Process -FilePath 'powershell.exe' -Verb RunAs -Wait -PassThru `
      -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', $encoded)
  } catch {
    Fail "No se concedieron permisos de administrador. Vuelve a ejecutar el instalador y acepta el aviso de UAC."
  }
  Remove-Item $tmp -Force -ErrorAction SilentlyContinue
  if ($p.ExitCode -eq 0) {
    Write-Host ""
    Write-Host "La instalacion termino en la ventana de administrador." -ForegroundColor Green
    if (Test-Path $HubCfgPath) { Write-Host "Config para el hub guardada en: $HubCfgPath" -ForegroundColor Cyan }
  } else {
    Write-Host "La instalacion fallo en la ventana de administrador (codigo $($p.ExitCode))." -ForegroundColor Red
  }
}

# Usuario de escritorio que va a ejecutar el agente (no el admin del UAC).
function Get-TargetUser {
  if ($env:ASTREA_USER) { return $env:ASTREA_USER }
  try {
    $u = (Get-CimInstance Win32_ComputerSystem).UserName
    if ($u) { return $u }
  } catch {}
  return "$env:USERDOMAIN\$env:USERNAME"
}

# ---------------------------------------------------------------------------
# 2. Tailscale
# ---------------------------------------------------------------------------
function Find-Tailscale {
  $candidates = @()
  $cmd = Get-Command tailscale.exe -ErrorAction SilentlyContinue
  if ($cmd) { $candidates += $cmd.Source }
  # ProgramW6432 apunta a "Program Files" real aunque PowerShell sea de 32 bits.
  foreach ($base in @($env:ProgramW6432, $env:ProgramFiles)) {
    if ($base) { $candidates += (Join-Path $base 'Tailscale\tailscale.exe') }
  }
  foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
  return $null
}

function Install-Tailscale {
  Step "Instalando Tailscale (no estaba instalado)"
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
  $msi = Join-Path $env:TEMP "tailscale-setup-$arch.msi"
  Get-WebFile "https://pkgs.tailscale.com/stable/tailscale-setup-latest-$arch.msi" $msi
  $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList @('/i', "`"$msi`"", '/quiet', '/norestart')
  Remove-Item $msi -Force -ErrorAction SilentlyContinue
  # 3010 = instalado, requiere reinicio (no hace falta para seguir).
  if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
    Fail "El instalador de Tailscale fallo (msiexec codigo $($p.ExitCode))."
  }
  $ts = $null
  for ($i = 0; $i -lt 30 -and -not $ts; $i++) { Start-Sleep 1; $ts = Find-Tailscale }
  if (-not $ts) { Fail "Tailscale se instalo pero no encuentro tailscale.exe." }
  Ok "Tailscale instalado"
  return $ts
}

function Wait-TailscaleService {
  $svc = Get-Service -Name Tailscale -ErrorAction SilentlyContinue
  if (-not $svc) { return }
  if ($svc.StartType -eq 'Disabled') { Set-Service -Name Tailscale -StartupType Automatic }
  if ($svc.Status -ne 'Running') { Start-Service -Name Tailscale }
  $svc.WaitForStatus('Running', [TimeSpan]::FromSeconds(30))
}

function Get-TailnetIP([string]$ts) {
  $r = Invoke-Native $ts @('ip', '-4')
  if ($r.Code -ne 0) { return $null }
  return ($r.Output | ForEach-Object { "$_".Trim() } | Where-Object { $_ -match '^100\.\d+\.\d+\.\d+$' } | Select-Object -First 1)
}

function Connect-Tailscale([string]$ts) {
  $ip = Get-TailnetIP $ts
  if ($ip) { return $ip }

  Step "Conectando esta PC a tu tailnet"
  if ($env:ASTREA_TS_AUTHKEY) {
    $null = Invoke-Native $ts @('up', '--unattended', "--auth-key=$env:ASTREA_TS_AUTHKEY")
  } else {
    Write-Host "   Inicia sesion con la MISMA cuenta de Tailscale que tu PC principal."
    Write-Host "   Si no se abre el navegador, copia el enlace que aparece abajo." -ForegroundColor Yellow
    # --unattended: el tailnet sigue activo aunque cambie la sesion de Windows.
    # Bloquea hasta que termines de iniciar sesion.
    $old = $ErrorActionPreference; $ErrorActionPreference = 'Continue'
    try { & $ts up --unattended | Out-Host } finally { $ErrorActionPreference = $old }
  }
  for ($i = 0; $i -lt 60; $i++) {
    $ip = Get-TailnetIP $ts
    if ($ip) { return $ip }
    Start-Sleep 2
  }
  Fail "Tailscale no obtuvo IP. Abre Tailscale desde la bandeja del sistema, inicia sesion y vuelve a ejecutar el instalador."
}

# ---------------------------------------------------------------------------
# 3. Agente
# ---------------------------------------------------------------------------
function Stop-Agent {
  foreach ($t in @($TaskName, $GuiTask)) { Stop-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue }
  Get-Process -Name 'astrea-agent' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800
}

function Install-AgentBinary {
  Step "Descargando el agente"
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  $tmp = Join-Path $env:TEMP 'astrea-agent.exe.download'
  Get-WebFile "$ReleaseUrl/astrea-agent.exe" $tmp

  $sums = Join-Path $env:TEMP 'astrea-SHA256SUMS.txt'
  $want = $null
  try {
    Get-WebFile "$ReleaseUrl/SHA256SUMS.txt" $sums
    $line = Get-Content $sums | Where-Object { $_ -match '\s\*?astrea-agent\.exe$' } | Select-Object -First 1
    if ($line) { $want = ($line -split '\s+')[0].ToLower() }
  } catch {
    Warn "No se pudo descargar SHA256SUMS.txt: $($_.Exception.Message)"
  } finally { Remove-Item $sums -Force -ErrorAction SilentlyContinue }
  if ($want) {
    $got = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLower()
    if ($want -ne $got) {
      Remove-Item $tmp -Force
      Fail "El binario descargado no coincide con SHA256SUMS.txt (descarga corrupta o manipulada). Reintenta."
    }
    Ok "SHA256 verificado"
  } else {
    Warn "Sin checksum para verificar el binario"
  }

  Stop-Agent
  Move-Item -Path $tmp -Destination $ExePath -Force
  Unblock-File -Path $ExePath -ErrorAction SilentlyContinue   # quita la marca "descargado de Internet"

  Start-Sleep -Seconds 2
  if (-not (Test-Path $ExePath)) {
    Fail ("Windows Defender (u otro antivirus) ha borrado astrea-agent.exe. Revisa 'Proteccion contra virus y amenazas' > Historial de proteccion, " +
          "permitelo, o anade una exclusion (PowerShell admin): Add-MpPreference -ExclusionPath '$InstallDir' ; luego reintenta.")
  }
  Ok "Agente en $ExePath"
}

function Write-AgentConfig([string]$ip) {
  Step "Configuracion"
  $token = $env:ASTREA_TOKEN
  $old = $null
  if (Test-Path $CfgPath) {
    try { $old = Get-Content -Raw $CfgPath | ConvertFrom-Json } catch {}
  }
  if (-not $token -and $old -and $old.token) {
    $token = $old.token
    Ok "Conservo el token existente (el hub ya configurado sigue valiendo)"
  }
  if (-not $token) {
    $bytes = New-Object byte[] 32
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $token = ($bytes | ForEach-Object { $_.ToString('x2') }) -join ''
  }
  # yolo = la IA puede usar todas las capacidades; readonly = solo observar.
  # keep_awake auto = no suspender mientras corre un servicio/job (modelos).
  $cfg = [ordered]@{ addr = "${ip}:$Port"; token = $token; data_dir = $InstallDir; mode = 'yolo'; keep_awake = 'auto' }
  if ($old) {
    # Respeta lo que el usuario haya cambiado a mano.
    foreach ($k in 'mode', 'keep_awake', 'allow', 'deny') {
      if ($null -ne $old.$k) { $cfg[$k] = $old.$k }
    }
  }
  $json = $cfg | ConvertTo-Json
  # Sin BOM: Set-Content -Encoding UTF8 de PS 5.1 anade BOM.
  [IO.File]::WriteAllText($CfgPath, $json, (New-Object Text.UTF8Encoding $false))
  Ok "Config escrita en $CfgPath"
  return $token
}

# El agente corre como el usuario de escritorio: necesita escribir logs y
# endpoints en la carpeta; y el token no debe ser legible por otros usuarios.
function Set-Permissions([string]$user) {
  $r = Invoke-Native icacls.exe @($InstallDir, '/grant', "${user}:(OI)(CI)M", '/Q')
  if ($r.Code -ne 0) { Warn "No pude dar permisos de escritura a $user en $InstallDir" }
  foreach ($f in @($CfgPath, $HubCfgPath)) {
    if (-not (Test-Path $f)) { continue }
    # SIDs en vez de nombres: "Administradores" / "SYSTEM" cambian con el idioma.
    $r = Invoke-Native icacls.exe @($f, '/inheritance:r', '/grant:r', '*S-1-5-32-544:F', '*S-1-5-18:F', "${user}:R", '/Q')
    if ($r.Code -ne 0) { Warn "No pude restringir permisos de $f" }
  }
}

function Set-FirewallRule {
  Step "Firewall de Windows"
  try {
    # Borra reglas previas del agente, incluidas las de "Bloquear" que crea
    # Windows si alguien cancelo el aviso del firewall: ganan a cualquier Allow.
    Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    Get-NetFirewallApplicationFilter -Program $ExePath -ErrorAction SilentlyContinue | Get-NetFirewallRule -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow -Program $ExePath `
      -Protocol TCP -LocalPort $Port -RemoteAddress '100.64.0.0/10' -Profile Any | Out-Null
    Ok "Puerto $Port abierto solo para el tailnet (100.64.0.0/10)"
  } catch {
    Warn "No pude crear la regla de firewall ($($_.Exception.Message)). Si usas otro firewall/antivirus, permite TCP $Port entrante desde 100.64.0.0/10."
  }
}

function New-AgentTaskSettings {
  New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
}

# Modo clasico: un solo proceso en la sesion de escritorio, al iniciar sesion.
function Register-ClassicTask([string]$user) {
  $action    = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`"" -WorkingDirectory $InstallDir
  $trigger   = New-ScheduledTaskTrigger -AtLogOn -User $user
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
  Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings (New-AgentTaskSettings) | Out-Null
  Start-ScheduledTask -TaskName $TaskName
  Ok "Tarea '$TaskName' (modo clasico: solo con la sesion iniciada) registrada para $user y arrancada"
}

function Register-AgentTask([string]$user) {
  Step "Arranque automatico"
  foreach ($t in @($TaskName, $GuiTask)) {
    Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  }
  if (-not $Headless) { Register-ClassicTask $user; return }

  # 1) Agente headless: al encender la PC, como TU usuario aunque nadie inicie
  #    sesion. S4U = "ejecutar tanto si el usuario inicio sesion como si no"
  #    sin guardar contrasena. Al ser tu usuario (no SYSTEM), HOME, la cache de
  #    HuggingFace, los modelos de ollama y lo instalado con winget son los
  #    mismos que con la sesion iniciada. Corre en la sesion 0: no ve el
  #    escritorio; eso lo hace el ayudante GUI.
  $gui = "--gui-addr 127.0.0.1:$GuiPort"
  try {
    $action    = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`" --role headless $gui" -WorkingDirectory $InstallDir
    $trigger   = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Highest
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings (New-AgentTaskSettings) -ErrorAction Stop | Out-Null
  } catch {
    Warn "Windows no acepto la tarea sin sesion para $user ($($_.Exception.Message))."
    Warn "Uso el modo clasico: el agente solo funcionara con la sesion iniciada."
    Register-ClassicTask $user
    return
  }

  # 2) Ayudante GUI: al iniciar sesion, dentro del escritorio, solo en loopback.
  $action    = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`" --role gui $gui" -WorkingDirectory $InstallDir
  $trigger   = New-ScheduledTaskTrigger -AtLogOn -User $user
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Highest
  Register-ScheduledTask -TaskName $GuiTask -Action $action -Trigger $trigger -Principal $principal -Settings (New-AgentTaskSettings) | Out-Null

  Start-ScheduledTask -TaskName $TaskName
  Start-ScheduledTask -TaskName $GuiTask
  Ok "Tarea '$TaskName' (al encender, sin sesion) y '$GuiTask' (pantalla/mouse/teclado al iniciar sesion) registradas para $user"
}

function Test-Agent([string]$ip) {
  Step "Comprobando que el agente responde"
  for ($i = 0; $i -lt 20; $i++) {
    try {
      $r = Invoke-WebRequest -Uri "http://${ip}:$Port/healthz" -UseBasicParsing -TimeoutSec 2
      if ($r.Content -match 'ok') {
        Ok "Agente escuchando en ${ip}:$Port"
        if ($Headless -and (Get-ScheduledTask -TaskName $GuiTask -ErrorAction SilentlyContinue)) { Test-GuiHelper }
        return $true
      }
    } catch {}
    Start-Sleep 1
  }
  Warn "El agente no responde en ${ip}:$Port."
  if (-not (Get-Process -Name 'astrea-agent' -ErrorAction SilentlyContinue)) { Warn "El proceso astrea-agent no esta corriendo." }
  if (Test-Path $LogPath) {
    Write-Host "   Ultimas lineas de ${LogPath}:" -ForegroundColor Yellow
    Get-Content $LogPath -Tail 10 | ForEach-Object { Write-Host "     $_" }
  }
  return $false
}

function Test-GuiHelper {
  for ($i = 0; $i -lt 10; $i++) {
    try {
      $r = Invoke-WebRequest -Uri "http://127.0.0.1:$GuiPort/healthz" -UseBasicParsing -TimeoutSec 2
      if ($r.Content -match 'ok') { Ok "Ayudante GUI activo (pantalla/mouse/teclado)"; return }
    } catch {}
    Start-Sleep 1
  }
  Warn "El ayudante GUI no responde en 127.0.0.1:${GuiPort}: pantalla/mouse/teclado no estaran disponibles hasta el proximo inicio de sesion. Mira $(Join-Path $InstallDir 'agent-gui.log')."
}

function Write-Summary([string]$ip, [string]$token) {
  $hub = @"
# --- En tu PC principal (Linux) ---------------------------------------
# 1) ~/.hermes/config.yaml, bajo mcp_servers:
  astrea:
    command: /home/TU_USUARIO/.local/bin/astrea-hub   # ruta que imprimio 'install.sh hub'
    args:
      - "--ws"
      - "ws://${ip}:$Port/ws"
      - "--caps"
      - "http://${ip}:$Port/capabilities"
      - "--token"
      - "$token"
    enabled: true

# 2) Panel de recursos (CPU / RAM / grafica):
astrea-panel --ws ws://${ip}:$Port/ws --token $token
"@
  [IO.File]::WriteAllText($HubCfgPath, $hub, (New-Object Text.UTF8Encoding $false))
  Write-Host ""
  Write-Host "==================== LISTO ====================" -ForegroundColor Cyan
  Write-Host "Esta PC ($env:COMPUTERNAME, $ip) ya es controlable por tu agente." -ForegroundColor Green
  Write-Host $hub
  Write-Host "Copia guardada en $HubCfgPath (solo legible por administradores y $(Get-TargetUser))." -ForegroundColor Yellow
}

function Uninstall-Astrea {
  Step "Desinstalando Astrea"
  Stop-Agent
  foreach ($t in @($TaskName, $GuiTask)) {
    Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  }
  Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  Remove-Item -Recurse -Force $InstallDir -ErrorAction SilentlyContinue
  Ok "Agente, tarea, regla de firewall y $InstallDir eliminados (Tailscale se queda)."
}

function Install-Astrea {
  Write-Host "== Astrea installer ==" -ForegroundColor Cyan
  Assert-Prerequisites
  if (-not (Test-Admin)) { Start-Elevated; return }
  if ($env:ASTREA_UNINSTALL -eq '1') { Uninstall-Astrea; return }

  $user = Get-TargetUser
  Write-Host "   Usuario de escritorio: $user"

  Step "Tailscale"
  $ts = Find-Tailscale
  if (-not $ts) { $ts = Install-Tailscale }
  Wait-TailscaleService
  $ip = Connect-Tailscale $ts
  Ok "IP del tailnet: $ip"

  Install-AgentBinary
  $token = Write-AgentConfig $ip
  Set-FirewallRule
  Register-AgentTask $user
  $alive = Test-Agent $ip
  Write-Summary $ip $token
  Set-Permissions $user
  if (-not $alive) { Fail "Instalado, pero el agente no responde. Revisa el log de arriba." }
}

$failed = $false
try {
  Install-Astrea
} catch {
  $failed = $true
  Write-Host ""
  Write-Host "ERROR: $($_.Exception.Message)" -ForegroundColor Red
} finally {
  if ($Elevated) {
    Write-Host ""
    Read-Host "Pulsa Enter para cerrar esta ventana" | Out-Null
  }
}
if ($Elevated) { exit ([int]$failed) }
}

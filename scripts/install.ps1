<#
.SYNOPSIS
  Instalador de Astrea para Windows. Deja la PC lista para ser controlada por
  tu agente (Hermes) sobre tu tailnet.

.DESCRIPTION
  Pensado para funcionar en un Windows recien instalado:
  - Pide permisos de administrador una sola vez (UAC) y sigue en esa ventana.
    Tambien corre desatendido como SYSTEM (script de arranque por GPO, Intune,
    PDQ...): no pregunta nada y deja el log en C:\ProgramData\Astrea\install.log.
  - Desde una memoria USB: deja install.cmd, install.ps1, un install.env (las
    variables de abajo, una por linea, CLAVE=valor) y opcionalmente
    astrea-agent.exe en la misma carpeta; doble clic en install.cmd y listo.
  - Instala Tailscale si falta (MSI oficial, sin depender de winget) y te
    guia para iniciar sesion.
  - Descarga el agente del ultimo release y verifica su SHA256.
  - Escribe la config (UTF-8 sin BOM) conservando el token si ya existia.
  - Abre el puerto en el Firewall de Windows solo para el tailnet.
  - Registra dos tareas programadas:
      AstreaAgent     al encender la PC, sin que nadie inicie sesion (modo
                      headless, como SYSTEM): comandos, servicios (modelos),
                      descargas. Tras un reinicio los modelos vuelven solos.
      AstreaAgentGUI  al iniciar sesion cualquier administrador: pantalla, mouse y teclado.
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
    $env:ASTREA_KIOSK_USER  -> modo aula: deja esa cuenta (p. ej. 'Aluno(a)') como administrador
                               SIN contrasena y con autologon, para que la PC arranque sola en un
                               escritorio usable. La cuenta tiene que existir. INSEGURO (acceso
                               fisico = entra); solo para equipos de laboratorio bajo tu control.
                               Vacio = no toca cuentas.
    $env:ASTREA_NO_AV_EXCLUSION=1 -> no anadir C:\ProgramData\Astrea a las exclusiones de Windows
                               Defender (por defecto se anade: Defender borraba el agente sin firmar)
    $env:ASTREA_UNINSTALL=1 -> desinstala el agente
    $env:ASTREA_LOCAL_EXE   -> ruta a un astrea-agent.exe ya presente (lo usa el MSI; no descarga)
    $env:ASTREA_ADDR        -> IP en la que escucha el agente SIN usar Tailscale (LAN o pruebas;
                               p. ej. el CI instala el MSI con 127.0.0.1). Normalmente vacio.
    $env:ASTREA_MSI=1       -> lo fija el MSI: corre como SYSTEM sin consola, asi que no
                               pregunta nada, no abre el login de Tailscale y deja el log en
                               C:\ProgramData\Astrea\install.log
#>

& {
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # la barra de progreso de PS 5.1 hace las descargas 10x mas lentas

# Carpeta desde la que se lanzo el instalador (USB, recurso compartido...), o
# $null si vino por 'irm | iex'.
function Get-ScriptDir {
  foreach ($p in @($env:ASTREA_SCRIPT, $PSCommandPath)) {
    if ($p -and (Test-Path $p)) { return Split-Path -Parent (Resolve-Path $p).Path }
  }
  return $null
}

# install.env junto al script: CLAVE=valor por linea (# comenta). Asi se
# despliega desde una USB o por GPO sin escribir variables a mano. Lo que ya
# este definido en el entorno gana. Va antes de leer ninguna variable.
function Import-EnvFile {
  $dir = Get-ScriptDir
  if (-not $dir) { return }
  $f = Join-Path $dir 'install.env'
  if (Test-Path $f) {
    foreach ($line in [IO.File]::ReadAllLines($f, [Text.Encoding]::UTF8)) {
      $l = $line.Trim()
      if (-not $l -or $l.StartsWith('#') -or $l -notmatch '^(ASTREA_[A-Z0-9_]+)\s*=\s*(.*)$') { continue }
      $k = $Matches[1]; $v = $Matches[2].Trim().Trim('"').Trim("'")
      if (-not [Environment]::GetEnvironmentVariable($k)) { [Environment]::SetEnvironmentVariable($k, $v) }
    }
  }
  # Un astrea-agent.exe en la misma carpeta se instala sin descargar (sin Internet).
  $exe = Join-Path $dir 'astrea-agent.exe'
  if (-not $env:ASTREA_LOCAL_EXE -and (Test-Path $exe)) { $env:ASTREA_LOCAL_EXE = $exe }
}
Import-EnvFile

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
$Msi        = $env:ASTREA_MSI -eq '1'
$InstallLog = Join-Path $InstallDir 'install.log'
# SYSTEM = script de arranque de GPO, Intune, PDQ, el MSI...: no hay nadie
# delante, asi que no se pregunta nada y todo va a $InstallLog.
$AsSystem   = $false
try { $AsSystem = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value -eq 'S-1-5-18' } catch {}
$Unattended = $Msi -or $AsSystem
$SetupTask  = 'AstreaSetup'   # tarea temporal de versiones anteriores; se borra al desinstalar
# Modo aula (opcional): deja una cuenta como administrador sin contrasena y con
# autologon, para que la PC arranque sola en un escritorio usable sin tocarla.
# Es un downgrade de seguridad deliberado para equipos de laboratorio bajo tu
# control; vacio = no se toca ninguna cuenta.
$KioskUser  = $env:ASTREA_KIOSK_USER

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

  # El proceso elevado no hereda las variables de entorno (UAC con otra cuenta):
  # se pasan explicitas TODAS las ASTREA_*. Antes se pasaba una lista fija y se
  # perdian ASTREA_KIOSK_USER, ASTREA_ADDR o ASTREA_LOCAL_EXE por el camino.
  $vars = [ordered]@{}
  Get-ChildItem env: | Where-Object { $_.Name -like 'ASTREA_*' } | ForEach-Object { $vars[$_.Name] = $_.Value }
  $vars['ASTREA_ELEVATED'] = '1'
  if (-not $vars['ASTREA_USER']) { $vars['ASTREA_USER'] = "$env:USERDOMAIN\$env:USERNAME" }
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
  $console = $null
  try { $console = (Get-CimInstance Win32_ComputerSystem).UserName } catch {}
  # El MSI corre como SYSTEM: nos pasa quien lo abrio ([LogonUser], sin dominio).
  # Si coincide con el usuario de la consola, usamos este, que trae el dominio.
  if ($env:ASTREA_MSI_USER) {
    if ($console -and ($console -split '\\')[-1] -eq $env:ASTREA_MSI_USER) { return $console }
    return $env:ASTREA_MSI_USER
  }
  if ($console) { return $console }
  # Cuenta de equipo (SYSTEM sin nadie con sesion: GPO de arranque, Intune...):
  # en modo headless no hace falta usuario, el agente corre como SYSTEM.
  if ($env:USERNAME -like '*$') { return $null }
  return "$env:USERDOMAIN\$env:USERNAME"
}

# Nombre del grupo Administradores: cambia con el idioma, se resuelve por SID.
function Get-AdminGroupName {
  return (New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-544').Translate([Security.Principal.NTAccount]).Value.Split('\')[-1]
}

# Miembro directo del grupo Administradores (no mira grupos anidados).
function Test-LocalAdmin([string]$account) {
  $acct = ($account -split '\\')[-1]
  $members = (Invoke-Native net.exe @('localgroup', (Get-AdminGroupName))).Output | ForEach-Object { "$_".Trim() }
  foreach ($m in $members) {
    if ($m -eq $account -or ($m -split '\\')[-1] -eq $acct) { return $true }
  }
  return $false
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
  if ($Unattended -and -not $env:ASTREA_TS_AUTHKEY) {
    # Como SYSTEM y sin ventana no hay navegador para iniciar sesion: el MSI se
    # quedaria colgado esperando.
    if ($Msi) { Fail "Tailscale no esta conectado. Abre Tailscale desde la bandeja del sistema, inicia sesion con la misma cuenta que tu PC principal y vuelve a abrir el MSI." }
    Fail "Tailscale no esta conectado y no hay ventana para iniciar sesion (instalacion desatendida). Define ASTREA_TS_AUTHKEY (install.env o variable del script de despliegue)."
  }

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

# Defender borraba o bloqueaba astrea-agent.exe (sin firmar, control remoto):
# como ya somos administrador, se excluye la carpeta antes de poner el binario.
function Add-AvExclusion {
  if ($env:ASTREA_NO_AV_EXCLUSION -eq '1') { return }
  if (-not (Get-Command Add-MpPreference -ErrorAction SilentlyContinue)) { return }
  try {
    Add-MpPreference -ExclusionPath $InstallDir -ErrorAction Stop
    Add-MpPreference -ExclusionProcess $ExePath -ErrorAction Stop
    Ok "Carpeta $InstallDir excluida de Windows Defender"
  } catch {
    # Proteccion contra alteraciones, Defender apagado por otro antivirus o
    # exclusiones gestionadas por directiva: no es fatal.
    Warn "No pude anadir la exclusion de Defender ($($_.Exception.Message)). Si el antivirus borra el agente, excluye $InstallDir a mano."
  }
}

function Remove-AvExclusion {
  if (-not (Get-Command Remove-MpPreference -ErrorAction SilentlyContinue)) { return }
  try {
    Remove-MpPreference -ExclusionPath $InstallDir -ErrorAction SilentlyContinue
    Remove-MpPreference -ExclusionProcess $ExePath -ErrorAction SilentlyContinue
  } catch {}
}

function Install-AgentBinary {
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  Add-AvExclusion
  # Si el MSI (o el usuario) ya dejó el binario, úsalo sin descargar nada.
  if ($env:ASTREA_LOCAL_EXE -and (Test-Path $env:ASTREA_LOCAL_EXE)) {
    Step "Usando el agente incluido"
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Stop-Agent
    if ((Resolve-Path $env:ASTREA_LOCAL_EXE).Path -ne (Join-Path $InstallDir 'astrea-agent.exe')) {
      Copy-Item -Path $env:ASTREA_LOCAL_EXE -Destination $ExePath -Force
    }
    Unblock-File -Path $ExePath -ErrorAction SilentlyContinue
    Ok "Agente en $ExePath"
    return
  }
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
    foreach ($k in 'mode', 'keep_awake', 'auto_update', 'allow', 'deny') {
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
# En modo headless el agente es SYSTEM y el ayudante GUI un administrador: no
# hace falta dar nada a $user (puede ser $null si se instalo como SYSTEM).
function Set-Permissions([string]$user) {
  $classicUser = if ($user -and -not $Headless) { $user } else { $null }
  if ($classicUser) {
    $r = Invoke-Native icacls.exe @($InstallDir, '/grant', "${classicUser}:(OI)(CI)M", '/Q')
    if ($r.Code -ne 0) { Warn "No pude dar permisos de escritura a $classicUser en $InstallDir" }
  }
  foreach ($f in @($CfgPath, $HubCfgPath)) {
    if (-not (Test-Path $f)) { continue }
    # SIDs en vez de nombres: "Administradores" / "SYSTEM" cambian con el idioma.
    $grants = @('*S-1-5-32-544:F', '*S-1-5-18:F')
    if ($classicUser) { $grants += "${classicUser}:R" }
    $r = Invoke-Native icacls.exe (@($f, '/inheritance:r', '/grant:r') + $grants + @('/Q'))
    if ($r.Code -ne 0) { Warn "No pude restringir permisos de $f" }
  }
}

# Modo aula: deja $kuser como administrador, sin contrasena y con autologon, para
# que la PC vuelva sola a un escritorio usable tras cualquier reinicio, sin que
# nadie la toque. Es inseguro a proposito (cualquiera con acceso fisico entra);
# solo se ejecuta si defines $env:ASTREA_KIOSK_USER. Idempotente.
function Set-KioskAutologon([string]$kuser) {
  Step "Modo aula para '$kuser' (admin sin contrasena + autologon)"
  $acct = ($kuser -split '\\')[-1]
  # net.exe y no Get-LocalUser: el modulo LocalAccounts no existe en
  # PowerShell de 32 bits. No se crea la cuenta: si falta, es un error de
  # ASTREA_KIOSK_USER y se avisa.
  if ((Invoke-Native net.exe @('user', $acct)).Code -ne 0) {
    Warn "La cuenta '$acct' no existe en esta PC: no configuro el modo aula. Corrige ASTREA_KIOSK_USER."; return
  }
  # Activa y sin exigir contrasena (si alguien la desactivo, el autologon falla).
  $null = Invoke-Native net.exe @('user', $acct, '/active:yes', '/passwordreq:no', '/expires:never')

  # Administrador: el grupo se resuelve por SID (S-1-5-32-544), su nombre cambia
  # con el idioma ("Administrators" vs "Administradores"). Fallaba al añadir por
  # nombre fijo en PCs con Windows en ingles.
  $grp = Get-AdminGroupName
  if (-not (Test-LocalAdmin $acct)) {
    $r = Invoke-Native net.exe @('localgroup', $grp, $acct, '/add')
    if ($r.Code -ne 0) { Warn "No pude añadir '$acct' a ${grp}: $($r.Output)" } else { Ok "'$acct' ahora es administrador ($grp)" }
  } else { Ok "'$acct' ya era administrador ($grp)" }

  # Contrasena en blanco: asi el agente desbloquea la sesion sin credenciales.
  # Por ADSI y no 'net user cuenta ""': PowerShell 5.1 no pasa argumentos
  # vacios a programas nativos, y net solo mostraba la cuenta sin tocarla.
  try {
    ([ADSI]"WinNT://$env:COMPUTERNAME/$acct,user").SetPassword('')
    Ok "'$acct' sin contrasena"
  } catch {
    Warn "No pude dejar '$acct' sin contrasena ($($_.Exception.InnerException.Message)). Puede que una directiva exija longitud minima."
  }

  # Permitir autologon con cuenta sin contrasena (Windows lo bloquea por defecto).
  $pl = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\PasswordLess\Device'
  if (-not (Test-Path $pl)) { New-Item $pl -Force | Out-Null }
  Set-ItemProperty $pl DevicePasswordLessBuildVersion 0

  $w = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
  Set-ItemProperty $w AutoAdminLogon '1'
  Set-ItemProperty $w DefaultUserName $acct
  Set-ItemProperty $w DefaultDomainName $env:COMPUTERNAME
  Set-ItemProperty $w DefaultPassword ''
  Remove-ItemProperty $w AutoLogonCount -ErrorAction SilentlyContinue
  Remove-ItemProperty $w ForceAutoLogon -ErrorAction SilentlyContinue
  Ok "Autologon configurado para '$acct' al encender"
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

function New-AgentTaskSettings([string]$multi = 'IgnoreNew') {
  New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
    -MultipleInstances $multi -ExecutionTimeLimit ([TimeSpan]::Zero) `
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
  foreach ($t in @($TaskName, $GuiTask, $SetupTask)) {
    Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  }
  if (-not $Headless) {
    if (-not $user) { Fail "El modo clasico (ASTREA_HEADLESS=0) necesita un usuario de escritorio. Pasa `$env:ASTREA_USER='EQUIPO\usuario'." }
    Register-ClassicTask $user; return
  }

  # 1) Agente headless: al encender la PC, como SYSTEM, aunque nadie inicie
  #    sesion. No depende de ninguna cuenta: si alguien borra o recrea la
  #    cuenta que instalo, el agente sigue arrancando (con S4U quedaba atado al
  #    SID de esa cuenta y moria en el siguiente reinicio). Corre en la sesion
  #    0: no ve el escritorio; eso lo hace el ayudante GUI.
  $gui = "--gui-addr 127.0.0.1:$GuiPort"
  try {
    $action    = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`" --role headless $gui" -WorkingDirectory $InstallDir
    $trigger   = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'S-1-5-18' -LogonType ServiceAccount -RunLevel Highest
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings (New-AgentTaskSettings) -ErrorAction Stop | Out-Null
  } catch {
    Warn "Windows no acepto la tarea sin sesion: $($_.Exception.Message)"
    if (-not $user) { Fail "Windows no acepto la tarea como SYSTEM y no hay usuario de escritorio para el modo clasico." }
    Warn "Uso el modo clasico: el agente solo funcionara con la sesion iniciada."
    Register-ClassicTask $user
    return
  }

  # 2) Ayudante GUI: al iniciar sesion, dentro del escritorio, solo en loopback.
  #    Para CUALQUIER administrador que inicie sesion, no solo $user: si entra
  #    otra cuenta admin, sin esto se quedaba sin pantalla, raton ni teclado.
  #    Solo administradores: el ayudante lee el token de config.json. Parallel:
  #    cada admin con sesion tiene el suyo; el que no esta en la consola se aparta.
  $admins    = (New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-544').Translate([Security.Principal.NTAccount]).Value
  $action    = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`" --role gui $gui" -WorkingDirectory $InstallDir
  $trigger   = New-ScheduledTaskTrigger -AtLogOn
  $principal = New-ScheduledTaskPrincipal -GroupId $admins -RunLevel Highest
  Register-ScheduledTask -TaskName $GuiTask -Action $action -Trigger $trigger -Principal $principal -Settings (New-AgentTaskSettings 'Parallel') | Out-Null

  Start-ScheduledTask -TaskName $TaskName
  Start-ScheduledTask -TaskName $GuiTask
  Ok "Tarea '$TaskName' (al encender, sin sesion, como SYSTEM) y '$GuiTask' (pantalla/mouse/teclado, para cualquier administrador que inicie sesion) registradas"
}

# Por que una tarea no pudo lanzar el agente, a partir de su LastTaskResult
# (HRESULT de CreateProcess). Devuelve $null si no es un bloqueo reconocido.
function Get-TaskBlockReason([string]$task) {
  $info = Get-ScheduledTaskInfo -TaskName $task -ErrorAction SilentlyContinue
  if (-not $info) { return $null }
  $code = [uint32]([int64]$info.LastTaskResult -band 0xFFFFFFFFL)
  $hex = '0x{0:X8}' -f $code
  # Se compara el texto: en PowerShell un literal 0x800711C7 es un int32
  # negativo y nunca seria igual al codigo sin signo.
  switch ($hex) {
    '0x800711C7' {  # ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION (4551)
      return ("Windows bloqueo astrea-agent.exe porque no esta firmado (${hex}: 'una directiva de Control de aplicaciones bloqueo este archivo'). " +
              "Es Smart App Control o WDAC. Si la PC es tuya: Seguridad de Windows > Control de aplicaciones y navegador > Smart App Control > Desactivado " +
              "(no se puede volver a activar sin reinstalar Windows). Si la PC esta gestionada (escuela/empresa): pide al administrador una regla WDAC " +
              "por hash o publisher para $ExePath.")
    }
    '0x800700E1' {  # ERROR_VIRUS_INFECTED (225)
      return "Windows Defender (u otro antivirus) bloqueo astrea-agent.exe como amenaza ($hex). Revisa Seguridad de Windows > Historial de proteccion, permitelo o anade la exclusion: Add-MpPreference -ExclusionPath '$InstallDir'."
    }
    '0x800700E2' {  # ERROR_VIRUS_DELETED (226)
      return "El antivirus borro astrea-agent.exe ($hex). Restauralo desde Seguridad de Windows > Historial de proteccion, anade la exclusion Add-MpPreference -ExclusionPath '$InstallDir' y reinstala."
    }
    '0x800704EC' {  # ERROR_ACCESS_DISABLED_BY_POLICY (1260)
      return "Una directiva de grupo (AppLocker / Restricciones de software) impide ejecutar programas desde $InstallDir ($hex). Pide al administrador que permita $ExePath."
    }
    '0x80070002' {  # ERROR_FILE_NOT_FOUND
      return "La tarea no encuentra $ExePath ($hex): probablemente el antivirus lo borro. Revisa el Historial de proteccion y reinstala."
    }
  }
  return $null
}

function Show-TaskBlockReasons {
  $found = $false
  foreach ($t in @($TaskName, $GuiTask)) {
    $why = Get-TaskBlockReason $t
    if ($why) { Warn "Tarea ${t}: $why"; $found = $true }
  }
  return $found
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
  if (Show-TaskBlockReasons) { return $false }
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
  if (Show-TaskBlockReasons) { return }
  Warn "El ayudante GUI no responde en 127.0.0.1:${GuiPort}: pantalla/mouse/teclado no estaran disponibles hasta el proximo inicio de sesion. Mira $(Join-Path $InstallDir 'agent-gui.log')."
}

function Write-Summary([string]$ip, [string]$token, [string]$user) {
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

# 3) Lo mas facil: pega esta ficha entera en el panel (Anadir un equipo), o:
install.sh add-machine $($env:COMPUTERNAME) ${ip} $token
"@
  [IO.File]::WriteAllText($HubCfgPath, $hub, (New-Object Text.UTF8Encoding $false))
  Write-Host ""
  Write-Host "==================== LISTO ====================" -ForegroundColor Cyan
  Write-Host "Esta PC ($env:COMPUTERNAME, $ip) ya es controlable por tu agente." -ForegroundColor Green
  Write-Host $hub
  $readers = if ($user -and -not $Headless) { "administradores y $user" } else { 'administradores' }
  Write-Host "Copia guardada en $HubCfgPath (solo legible por $readers)." -ForegroundColor Yellow
}

# El ayudante GUI (pantalla/raton/teclado) solo arranca en sesiones de
# administradores: avisa si la cuenta que va a usar la PC no lo es.
function Show-GuiCoverage([string]$user) {
  if (-not $Headless) { return }
  foreach ($u in @($user, $KioskUser) | Where-Object { $_ } | Select-Object -Unique) {
    if (-not (Test-LocalAdmin $u)) {
      Warn "'$u' no es administrador local: en su sesion no habra pantalla/raton/teclado para la IA (comandos y modelos si). Hazla administradora o usa ASTREA_KIOSK_USER='$(($u -split '\\')[-1])'."
    }
  }
}

function Uninstall-Astrea {
  Step "Desinstalando Astrea"
  Stop-Agent
  foreach ($t in @($TaskName, $GuiTask, $SetupTask)) {
    Unregister-ScheduledTask -TaskName $t -Confirm:$false -ErrorAction SilentlyContinue
  }
  Get-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  Remove-AvExclusion
  Remove-Item -Recurse -Force $InstallDir -ErrorAction SilentlyContinue
  Ok "Agente, tareas, regla de firewall, exclusion de Defender y $InstallDir eliminados (Tailscale y el modo aula se quedan)."
}

function Install-Astrea {
  Write-Host "== Astrea installer ==" -ForegroundColor Cyan
  Assert-Prerequisites
  if (-not (Test-Admin)) { Start-Elevated; return }
  if ($env:ASTREA_UNINSTALL -eq '1') { Uninstall-Astrea; return }

  $me = [Security.Principal.WindowsIdentity]::GetCurrent().Name
  Write-Host "   Instalando como: $me$(if ($Unattended) { ' (desatendido)' })"
  $user = Get-TargetUser
  if ($user) { Write-Host "   Usuario de escritorio: $user" } else { Write-Host "   Nadie con sesion iniciada: el agente correra como SYSTEM igualmente" }

  if ($env:ASTREA_ADDR) {
    $ip = $env:ASTREA_ADDR
    Warn "ASTREA_ADDR=${ip}: sin Tailscale. El agente escucha en esa IP; la regla de firewall solo admite el tailnet (100.64.0.0/10), amplia la regla si lo usas en una LAN."
  } else {
    Step "Tailscale"
    $ts = Find-Tailscale
    if (-not $ts) {
      if ($Msi) {
        # Windows no permite lanzar el MSI de Tailscale dentro de otro MSI (1618).
        Fail "Tailscale no esta instalado. Instalalo desde https://tailscale.com/download, inicia sesion y vuelve a abrir el MSI de Astrea."
      }
      $ts = Install-Tailscale
    }
    Wait-TailscaleService
    $ip = Connect-Tailscale $ts
    Ok "IP del tailnet: $ip"
  }

  Install-AgentBinary
  $token = Write-AgentConfig $ip
  Set-FirewallRule
  # Antes de las tareas: la tarea GUI es para administradores, y el modo aula
  # es lo que convierte a esa cuenta en administradora.
  if ($KioskUser) { Set-KioskAutologon $KioskUser }
  Register-AgentTask $user
  Show-GuiCoverage $user
  $alive = Test-Agent $ip
  Write-Summary $ip $token $user
  Set-Permissions $user
  if (-not $alive) {
    # En el MSI, fallar aqui haria rollback de los archivos dejando las tareas
    # apuntando a un exe borrado. Mejor dejarlo instalado y avisar.
    if ($Msi) { Warn "Instalado, pero el agente no responde todavia. Mira $LogPath y docs/TROUBLESHOOTING.md."; return }
    Fail "Instalado, pero el agente no responde. Revisa el log de arriba."
  }
}

$failed = $false
$transcript = $false
if ($Unattended) {
  # Sin consola (MSI, o SYSTEM por GPO/Intune/PDQ): todo lo que imprime el
  # instalador va a un log. Al desinstalar va a TEMP, porque la carpeta de
  # Astrea se borra.
  $msiLog = if ($env:ASTREA_UNINSTALL -eq '1') { Join-Path $env:TEMP 'astrea-uninstall.log' } else { $InstallLog }
  try {
    New-Item -ItemType Directory -Force -Path (Split-Path $msiLog) | Out-Null
    Start-Transcript -Path $msiLog -Force | Out-Null
    $transcript = $true
  } catch {}
}
try {
  Install-Astrea
} catch {
  $failed = $true
  Write-Host ""
  Write-Host "ERROR: $($_.Exception.Message)" -ForegroundColor Red
} finally {
  if ($Elevated -and -not $Msi) {
    Write-Host ""
    Read-Host "Pulsa Enter para cerrar esta ventana" | Out-Null
  }
  if ($transcript) { try { Stop-Transcript | Out-Null } catch {} }
}
# Codigo de salida para quien lanzo el proceso (UAC, MSI, GPO/Intune/PDQ). En
# una consola interactiva normal no se hace exit: cerraria la ventana.
if ($Elevated -or $Unattended) { exit ([int]$failed) }
}

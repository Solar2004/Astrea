<#
.SYNOPSIS
  Instalador de Astrea para Windows. Deja la PC lista para ser controlada por
  tu agente (Hermes) sobre tu tailnet.

.DESCRIPTION
  - Descarga el binario del agente desde el último release de GitHub.
  - Escribe la config (token, IP del tailnet).
  - Registra una tarea programada que arranca el agente en tu sesión de
    escritorio al iniciar sesión (necesario para mouse/teclado/captura).
  - Imprime el token y la IP para configurar el hub en tu PC principal.

.EXAMPLE
  irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1 | iex

.NOTES
  Variables de entorno opcionales antes de ejecutar:
    $env:ASTREA_TOKEN  -> usa este token en vez de generar uno
    $env:ASTREA_PORT   -> puerto (default 7700)
    $env:ASTREA_REPO   -> owner/repo (default Solar2004/Astrea)
#>

$ErrorActionPreference = 'Stop'

$Repo    = if ($env:ASTREA_REPO)  { $env:ASTREA_REPO }  else { 'Solar2004/Astrea' }
$Port    = if ($env:ASTREA_PORT)  { $env:ASTREA_PORT }  else { '7700' }
$InstallDir = Join-Path $env:ProgramData 'Astrea'
$ExePath    = Join-Path $InstallDir 'astrea-agent.exe'
$CfgPath    = Join-Path $InstallDir 'config.json'
$TaskName   = 'AstreaAgent'

Write-Host "== Astrea installer ==" -ForegroundColor Cyan

# --- 1. Tailscale ---------------------------------------------------------
$ts = Get-Command tailscale.exe -ErrorAction SilentlyContinue
if (-not $ts) {
  $ts = Get-Command "$env:ProgramFiles\Tailscale\tailscale.exe" -ErrorAction SilentlyContinue
}
if (-not $ts) {
  Write-Host "Tailscale no está instalado." -ForegroundColor Yellow
  Write-Host "Instálalo (winget install tailscale.tailscale) e inicia sesión con la MISMA cuenta que tu PC principal, luego vuelve a ejecutar este script." -ForegroundColor Yellow
  if (Get-Command winget -ErrorAction SilentlyContinue) {
    $ans = Read-Host "¿Instalar Tailscale ahora con winget? (s/N)"
    if ($ans -eq 's') { winget install --id tailscale.tailscale -e --accept-source-agreements --accept-package-agreements }
  }
  Write-Host "Cuando Tailscale esté conectado, re-ejecuta el instalador." -ForegroundColor Yellow
  return
}

$TailnetIP = (& $ts.Source ip -4 2>$null | Select-Object -First 1).Trim()
if (-not $TailnetIP) {
  Write-Host "Tailscale está instalado pero no veo una IP del tailnet. Ejecuta 'tailscale up' e inicia sesión, luego reintenta." -ForegroundColor Yellow
  return
}
Write-Host "IP del tailnet: $TailnetIP" -ForegroundColor Green

# --- 2. Descargar el agente ----------------------------------------------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$Url = "https://github.com/$Repo/releases/latest/download/astrea-agent.exe"
Write-Host "Descargando agente desde $Url ..."
try {
  Invoke-WebRequest -Uri $Url -OutFile $ExePath -UseBasicParsing
} catch {
  Write-Host "No se pudo descargar el release. ¿Ya publicaste uno? Error: $_" -ForegroundColor Red
  return
}

# --- 3. Token y config ----------------------------------------------------
if ($env:ASTREA_TOKEN) {
  $Token = $env:ASTREA_TOKEN
} else {
  $bytes = New-Object byte[] 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  $Token = ($bytes | ForEach-Object { $_.ToString('x2') }) -join ''
}

$cfg = [ordered]@{
  addr     = "$TailnetIP`:$Port"
  token    = $Token
  data_dir = $InstallDir
} | ConvertTo-Json
Set-Content -Path $CfgPath -Value $cfg -Encoding UTF8
Write-Host "Config escrita en $CfgPath"

# --- 4. Tarea programada (sesión interactiva) -----------------------------
# Debe correr en la sesión de escritorio del usuario para poder mover el mouse
# y capturar pantalla; un servicio en sesión 0 no puede.
Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
$action   = New-ScheduledTaskAction -Execute $ExePath -Argument "--config `"$CfgPath`""
$trigger  = New-ScheduledTaskTrigger -AtLogOn
$principal = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Seconds 0)
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskName $TaskName
Write-Host "Tarea '$TaskName' registrada y arrancada." -ForegroundColor Green

# --- 5. Resumen para el hub ----------------------------------------------
Write-Host ""
Write-Host "==================== LISTO ====================" -ForegroundColor Cyan
Write-Host "Esta PC ya es controlable por tu agente." -ForegroundColor Green
Write-Host ""
Write-Host "En tu PC principal, añade esto a ~/.hermes/config.yaml bajo mcp_servers:" -ForegroundColor Cyan
Write-Host @"
  astrea:
    command: /ruta/a/astrea-hub
    args:
      - "--ws"
      - "ws://$TailnetIP`:$Port/ws"
      - "--caps"
      - "http://$TailnetIP`:$Port/capabilities"
      - "--token"
      - "$Token"
    enabled: true
"@ -ForegroundColor White
Write-Host "Guarda este token en lugar seguro: $Token" -ForegroundColor Yellow

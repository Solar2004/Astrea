# Diagnóstico — si algo falla en Windows

Astrea deja todo en `C:\ProgramData\Astrea\`:

| Archivo          | Qué es                                                        |
|------------------|---------------------------------------------------------------|
| `agent.log`      | Log del agente headless (arranque, binds, errores, panics).   |
| `agent-gui.log`  | Log del ayudante GUI (pantalla/mouse/teclado).                |
| `audit.log`      | Una línea JSON por cada llamada: método, args (2 KiB), ok/err.|
| `config.json`    | addr, token, data_dir, mode, keep_awake, allow/deny.          |
| `hub-config.txt` | El bloque listo para pegar en el hub.                         |

## Lo primero: `--diagnose`

```powershell
& "$env:ProgramData\Astrea\astrea-agent.exe" --diagnose
```

Imprime versión, plataforma, la config **sin el token**, y el final de
`agent.log`, `agent-gui.log` y `audit.log`. Copia y pega esa salida: es lo que
necesito para ayudarte. También:

```powershell
& "$env:ProgramData\Astrea\astrea-agent.exe" --version
```

## Comprobaciones rápidas

```powershell
# ¿Están las tareas y corriendo?
Get-ScheduledTask AstreaAgent, AstreaAgentGUI | Format-Table TaskName, State
Get-Process astrea-agent -ErrorAction SilentlyContinue

# ¿El agente responde en el tailnet? (sustituye la IP 100.x.y.z)
curl.exe http://127.0.0.1:7700/healthz          # debe decir: ok

# Ver el log en vivo
Get-Content "$env:ProgramData\Astrea\agent.log" -Tail 50 -Wait
```

## Síntomas típicos

- **El agente no arranca al encender / tras reiniciar.** Mira `agent.log`. Si
  ves `listen … retrying (is Tailscale connected?)`, es normal mientras
  Tailscale asigna la IP; debe conectar en segundos. Si la tarea aparece en
  estado `Disabled` o no está, reinstala: `& ... install.ps1`.
- **La tarea headless no se registró** (Windows rechazó S4U). El instalador cae
  al modo clásico (una tarea al iniciar sesión) y lo avisa. Entonces los
  modelos solo corren con la sesión iniciada. Para forzar el modo clásico a
  propósito: `$env:ASTREA_HEADLESS=0` antes de instalar.
- **`screen.capture`/`mouse`/`key` dan `unavailable`.** No hay sesión de
  escritorio abierta (nadie logueado). Inicia sesión en esa PC, o usa el modo
  clásico. En `fleet.status` esa máquina sale `online` con `desktop:false`.
- **Windows Defender borró `astrea-agent.exe`.** Es un binario sin firmar que
  controla la PC; algunos antivirus lo marcan. Restáuralo desde el historial de
  protección y añade una exclusión para `C:\ProgramData\Astrea\`.
- **El hub no ve las herramientas.** Confirma `healthz`, que el token del
  `machines.json`/config del hub coincide con el de `config.json`, y que la IP
  del tailnet es la correcta. El hub reintenta solo y cachea las tools; si la PC
  estaba apagada, aparecen cuando vuelve.

## Enviarme los logs

Lo ideal es la salida de `--diagnose`. Si prefieres los archivos:

```powershell
Compress-Archive "$env:ProgramData\Astrea\*.log" "$env:USERPROFILE\Desktop\astrea-logs.zip"
```

y me pasas `astrea-logs.zip` (no contiene el token; sí los comandos ejecutados).

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

## El agente no arranca: Windows bloquea el .exe sin firmar

En un PC con **Smart App Control** o **WDAC/AppLocker** (habitual en Windows 11
nuevos y en PCs gestionados de empresa/aula), Windows no deja ejecutar un
binario sin firma reconocida. Síntoma: tareas registradas pero el agente nunca
corre, `agent.log` vacío, `healthz` sin respuesta. El instalador detecta el
`LastTaskResult` de la tarea y lo explica; la correspondencia:

| LastTaskResult | Causa | Qué hacer |
|----------------|-------|-----------|
| `0x800711C7` | Smart App Control / WDAC | Si la PC es tuya: apaga Smart App Control (abajo). Si está gestionada: pide al admin una regla WDAC por **publisher** o **hash**, o firma el binario. |
| `0x800700E1` / `0x800700E2` | Antivirus lo bloqueó/borró | Restaura el exe y excluye `C:\ProgramData\Astrea\`: `Add-MpPreference -ExclusionPath 'C:\ProgramData\Astrea'`. |
| `0x800704EC` | AppLocker / SRP | Regla de AppLocker del administrador. |
| `0x80070002` | El exe ya no existe | Reinstala. |

Comprobar Smart App Control (0=off, 1=ON, 2=evaluación):

```powershell
(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy' -Name VerifiedAndReputablePolicyState).VerifiedAndReputablePolicyState
```

**Apagar Smart App Control** (solo si la PC es tuya; es irreversible sin
restablecer Windows): `Start-Process "windowsdefender://appbrowser"` →
*Smart App Control* → *Desactivado*. Luego `Start-ScheduledTask AstreaAgent`.

Si la PC está gestionada por una organización (`dsregcmd /status` muestra
`DomainJoined`/`MDMUrl`), lo normal es que no puedas tocar estas políticas: la
única vía es firmar el agente con un certificado que el admin acepte.

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

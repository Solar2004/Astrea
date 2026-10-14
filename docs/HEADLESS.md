# Agente sin sesión (Windows)

Objetivo: que tras reiniciar la PC, **sin que nadie inicie sesión**, el agente
responda y los servicios con autostart (los modelos) vuelvan a levantarse. Y
que, cuando alguien inicia sesión, la pantalla, el mouse, el teclado y
`sys.idle` funcionen como siempre.

## Diseño

El instalador registra dos tareas programadas que lanzan el **mismo binario**
con roles distintos:

| Tarea            | Cuándo              | Cómo corre                                     | Rol                      | Escucha                   |
|------------------|---------------------|------------------------------------------------|--------------------------|---------------------------|
| `AstreaAgent`    | Al encender la PC   | Tu usuario, S4U (sin contraseña), elevado, sesión 0 | `--role headless`   | `<ip-tailnet>:7700` (como siempre) |
| `AstreaAgentGUI` | Al iniciar sesión   | Tu usuario, interactivo, elevado, tu escritorio | `--role gui`            | `127.0.0.1:7701` (solo loopback) |

```
hub ──tailnet──▶ AstreaAgent (headless, :7700)
                   ├─ exec, fs, services, jobs, downloads, deps, tasks, power…  (aquí)
                   └─ screen.*, mouse.*, key.*, sys.idle, gui.exec ──loopback──▶ AstreaAgentGUI (:7701)
```

- El hub sigue viendo **una sola máquina**: mismo puerto, token, protocolo y
  `/capabilities`. Las tools GUI siempre se anuncian.
- **Sin nadie conectado**, las tools GUI responden al momento con el código
  `unavailable` ("no interactive session: nobody is logged in…"), no se
  quedan colgadas. `sys.idle` no da error: devuelve `user_active: false,
  session: "none"`, porque si nadie ha iniciado sesión, nadie está usando la PC.
- `gui.exec` es nuevo: ejecuta un comando **dentro del escritorio** del
  usuario, para abrir ventanas visibles. `exec` corre en el agente headless:
  no depende de la sesión y lo que lanza sobrevive al cierre de sesión, pero
  las ventanas que abra son invisibles.
- El ayudante GUI usa la misma config (mismo token). No tiene endpoints,
  servicios ni audit log propios: el agente headless ya controla y audita
  cada llamada que le reenvía. Si le das una dirección que no sea loopback, se niega a arrancar.

### Por qué el mismo usuario (S4U) y no SYSTEM

Como SYSTEM cambiarían `%USERPROFILE%`, `%LOCALAPPDATA%` y el registro HKCU.
Lo que la IA instaló o descargó con la sesión iniciada (modelos de ollama en
`~\.ollama`, la caché de HuggingFace, los venv, los paquetes winget por
usuario) no estaría donde lo busca el agente tras un reinicio, y winget
apenas funciona como SYSTEM. S4U ejecuta **como tu usuario**, sin guardar la
contraseña: mismo perfil y mismo PATH, con o sin sesión. Por eso no hace
falta mover modelos a `ProgramData`.

Limitaciones de S4U: no tiene credenciales de red de Windows, así que no
puede acceder a carpetas compartidas SMB ni a recursos con Kerberos (Internet
y Tailscale funcionan con normalidad), y tampoco puede leer archivos cifrados
con EFS. Si Windows rechaza S4U para tu cuenta, el instalador avisa y vuelve
al modo clásico.

### Otras decisiones

- **winget sin sesión:** el `winget` del PATH es un alias de la Microsoft
  Store, y esos alias suelen fallar fuera del escritorio. `deps.ensure` usa
  el `winget.exe` real del paquete App Installer.
- **keep-awake:** `SetThreadExecutionState` afecta a todo el sistema y
  debería funcionar desde la sesión 0. Hay que verificarlo (ver checklist).
- **Modo clásico:** con `ASTREA_HEADLESS=0` se registra una sola tarea al
  iniciar sesión (`--role full`), como en v0.1. En Linux el rol por defecto
  sigue siendo `full`.
- **Autologon:** no hace falta con este diseño. Si algún día quieres que la
  pantalla esté disponible sin tocar la PC, puedes activarlo a mano con
  Sysinternals Autologon. **Es inseguro**: cualquiera con acceso físico
  entra en tu sesión, y la contraseña queda en el registro (LSA secret).

## Checklist de prueba manual (Windows real)

Requisitos: una PC Windows de prueba en el tailnet, y el hub o
`astrea-panel` en tu Linux. `PC` = IP del tailnet de la PC Windows.

**Instalación**
- [ ] Ejecuta el instalador. Debe mostrar "Tarea 'AstreaAgent' (al encender, sin sesion) y 'AstreaAgentGUI' … registradas", "Agente escuchando" y "Ayudante GUI activo".
- [ ] `Get-ScheduledTask AstreaAgent | Select -Expand Principal` → `LogonType: S4U`, `RunLevel: Highest`.
- [ ] Hay dos procesos `astrea-agent.exe` en el Administrador de tareas: uno en la sesión 0 y otro en tu sesión.
- [ ] Si viste "Windows no acepto la tarea sin sesion…", apunta el mensaje, tu tipo de cuenta (local o Microsoft) y la edición de Windows.

**Con la sesión iniciada (todo como antes)**
- [ ] `screen.capture` devuelve tu escritorio, `mouse.move` mueve el ratón y `key.type` escribe en el Bloc de notas.
- [ ] `sys.idle` muestra `idle_sec` y la ventana activa.
- [ ] `gui.exec {"command":"start notepad"}` abre un Bloc de notas **visible**.
- [ ] `exec {"command":"echo %USERPROFILE% & echo %LOCALAPPDATA% & whoami"}` → **tu** perfil y tu usuario (no `systemprofile`).
- [ ] `deps.check {"names":["git","python","uv"]}`: el resultado coincide con lo que tienes instalado.

**Reinicio sin iniciar sesión (el objetivo)**
- [ ] Define un servicio con autostart, por ejemplo llama-server con `expose`, y comprueba que responde.
- [ ] Reinicia la PC y **no inicies sesión**. Espera unos 2 minutos.
- [ ] Desde Linux: `curl http://PC:7700/healthz` → `ok`.
- [ ] `service.status` → el modelo está en `running` y `POST /dyn/<nombre>/v1/chat/completions` contesta.
- [ ] `screen.capture` → error `unavailable … no interactive session` **en menos de 3 s**.
- [ ] `sys.idle` → `{"user_active": false, "session": "none", …}`.
- [ ] `exec {"command":"echo %USERPROFILE%"}` → el mismo perfil que con la sesión iniciada.
- [ ] `exec {"command":"dir %USERPROFILE%\\.ollama\\models"}` (si usas ollama) → los modelos siguen ahí.
- [ ] `deps.ensure {"names":["winget:7zip.7zip"],"timeout_sec":600}` → instala sin sesión iniciada. Si falla, guarda la salida: indica si winget funciona en S4U.
- [ ] `net.download` de un archivo pequeño → `done`.
- [ ] keep-awake: con un servicio corriendo y la suspensión configurada a 1 min, la PC **no** se suspende. `powercfg /requests` (en una consola elevada, ya con la sesión iniciada) muestra a astrea-agent bajo SYSTEM.

**Iniciar y cerrar sesión**
- [ ] Inicia sesión: en unos segundos `screen.capture` vuelve a funcionar sin reiniciar nada.
- [ ] Cierra sesión: las tools GUI vuelven a `unavailable` y los servicios **siguen** corriendo.
- [ ] Bloquea la pantalla (Win+L): `screen.capture` puede salir negro o fallar (es el escritorio seguro de Windows; pasaba igual antes).

**Reinstalar y desinstalar**
- [ ] Reinstala encima: conserva el token, `mode`, `keep_awake`, `allow`, `deny` y los servicios definidos.
- [ ] `ASTREA_HEADLESS=0` + reinstalar → una sola tarea `AstreaAgent` al iniciar sesión (modo clásico) y no queda `AstreaAgentGUI`.
- [ ] `ASTREA_UNINSTALL=1` → borra las dos tareas, la regla de firewall y `C:\ProgramData\Astrea`.

Logs: `C:\ProgramData\Astrea\agent.log` (headless) y `agent-gui.log` (ayudante).

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
| `AstreaAgent`    | Al encender la PC   | SYSTEM, sesión 0                               | `--role headless`   | `<ip-tailnet>:7700` (como siempre) |
| `AstreaAgentGUI` | Al iniciar sesión cualquier administrador | Quien inicia sesión (grupo Administradores), interactivo, elevado, su escritorio | `--role gui`            | `127.0.0.1:7701` (solo loopback) |

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
  Antes de una acción de pantalla (captura, clic, tecla, `gui.exec`) el agente
  intenta iniciar sesión solo desde la pantalla de inicio, igual que desbloquea
  la de bloqueo: sirve para cuentas sin contraseña. Si no lo consigue, espera
  5 min, luego 10, 20… (hasta 2 h) antes de volver a intentarlo, para que una
  cuenta con contraseña no se bloquee por intentos fallidos.
- Si entra **otra cuenta administradora**, también tiene su ayudante por la
  tarea de inicio de sesión (lee el token de `config.json`, que solo pueden
  leer los administradores). Al cambiar de usuario, el ayudante que se queda en
  la sesión que ya no está en la consola se cierra solo. Los agentes anteriores
  a v0.6.5 registraban la tarea solo para la cuenta que instaló; desde v0.6.5 el
  agente la cambia solo al arrancar.
- **Cuenta que no es administradora (p. ej. un alumno):** la tarea de inicio de
  sesión no se dispara para ella, así que el agente headless (SYSTEM) lanza él
  mismo el ayudante en la sesión del alumno. Lo ejecuta **como ese alumno** pero
  con derechos de administrador metidos solo en el proceso: un inicio de sesión
  S4U (LsaLogonUser con el paquete MSV1_0) que le añade al token el grupo
  Administradores y el logon SID de la sesión, lo pasa a integridad alta y lo
  arranca en `winsta0\default`. **Nunca se toca la cuenta en el sistema**: no se
  mete en el grupo Administradores ni se pide contraseña. Como el alumno no
  puede leer `config.json`, el ayudante no usa el token del agente (si lo
  tuviera, el alumno controlaría el agente SYSTEM): el headless le pasa un
  secreto aleatorio por lanzamiento en la variable de entorno
  `ASTREA_GUI_SECRET`, y ese ayudante guarda su log y su portapapeles en
  `%LOCALAPPDATA%\Astrea`. Si el S4U falla, se lanza con el token normal del
  alumno (sin admin) y queda anotado en el log. Así el ayudante **sí** puede
  manejar ventanas elevadas; lo que **no** alcanza es el escritorio seguro de
  Windows (el aviso de UAC, el inicio de sesión). Riesgo aceptado en equipos de
  aula: lo que la IA abra en esa sesión corre con permisos de administrador y el
  alumno lo tiene delante.
- `gui.exec` es nuevo: ejecuta un comando **dentro del escritorio** del
  usuario, para abrir ventanas visibles. `exec` corre en el agente headless:
  no depende de la sesión y lo que lanza sobrevive al cierre de sesión, pero
  las ventanas que abra son invisibles.
- El ayudante GUI usa la misma config (mismo token). No tiene endpoints,
  servicios ni audit log propios: el agente headless ya controla y audita
  cada llamada que le reenvía. Si le das una dirección que no sea loopback, se niega a arrancar.

### Por qué SYSTEM y no tu usuario (S4U)

Hasta v0.6.4 el agente corría como la cuenta que lo instaló, con S4U (sin
contraseña). Eso lo ataba al SID de esa cuenta: si alguien la borraba y la
volvía a crear con el mismo nombre (pasó en la flota), la tarea apuntaba a un
SID que ya no existía y el agente no volvía a arrancar tras el siguiente
reinicio. Como SYSTEM no depende de ninguna cuenta.

Lo que cuesta: `%USERPROFILE%` y `%LOCALAPPDATA%` son los de SYSTEM
(`C:\Windows\System32\config\systemprofile`), no los tuyos. Lo que la IA
instaló o descargó como tu usuario (modelos de ollama en `~\.ollama`, la
caché de HuggingFace, venv, paquetes winget por usuario) no está donde lo
busca el agente; lo que instale el propio agente sí queda en su perfil y
sobrevive a los reinicios. Para algo compartido con tu sesión, usa rutas fijas
como `C:\ProgramData`. Si Windows rechaza la tarea, el instalador avisa y
vuelve al modo clásico.

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
- [ ] Ejecuta el instalador. Debe mostrar "Tarea 'AstreaAgent' (al encender, sin sesion, como SYSTEM) y 'AstreaAgentGUI' … registradas", "Agente escuchando" y "Ayudante GUI activo".
- [ ] `Get-ScheduledTask AstreaAgent | Select -Expand Principal` → `UserId: SYSTEM`, `RunLevel: Highest`.
- [ ] Hay dos procesos `astrea-agent.exe` en el Administrador de tareas: uno en la sesión 0 y otro en tu sesión.
- [ ] Si viste "Windows no acepto la tarea sin sesion…", apunta el mensaje, tu tipo de cuenta (local o Microsoft) y la edición de Windows.

**Con la sesión iniciada (todo como antes)**
- [ ] `screen.capture` devuelve tu escritorio, `mouse.move` mueve el ratón y `key.type` escribe en el Bloc de notas.
- [ ] `sys.idle` muestra `idle_sec` y la ventana activa.
- [ ] `gui.exec {"command":"start notepad"}` abre un Bloc de notas **visible**.
- [ ] `exec {"command":"whoami"}` → `nt authority\system`.
- [ ] `deps.check {"names":["git","python","uv"]}`: el resultado coincide con lo que tienes instalado.

**Reinicio sin iniciar sesión (el objetivo)**
- [ ] Define un servicio con autostart, por ejemplo llama-server con `expose`, y comprueba que responde.
- [ ] Reinicia la PC y **no inicies sesión**. Espera unos 2 minutos.
- [ ] Desde Linux: `curl http://PC:7700/healthz` → `ok`.
- [ ] `service.status` → el modelo está en `running` y `POST /dyn/<nombre>/v1/chat/completions` contesta.
- [ ] `screen.capture` → error `unavailable … no interactive session` **en menos de 3 s**.
- [ ] `sys.idle` → `{"user_active": false, "session": "none", …}`.
- [ ] `deps.ensure {"names":["winget:7zip.7zip"],"timeout_sec":600}` → instala sin sesión iniciada. Si falla, guarda la salida: indica si winget funciona como SYSTEM.
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

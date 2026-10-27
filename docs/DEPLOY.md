# Desplegar Astrea en PCs nuevos del aula

Objetivo: que cada PC nueva quede, de una sola pasada, con el agente corriendo
como SYSTEM (sobrevive a borrados de cuenta y reinicios) y con la cuenta
`Aluno(a)` como administrador sin contraseña y autologon, de modo que arranque
sola en un escritorio usable y controlable por el hub.

## Por qué hay que hacerlo en cada PC (no se puede en remoto)

Una PC solo entra en la flota **después** de unirse al tailnet, y para unirse
hay que ejecutar el instalador en ella. Antes de eso es invisible para el hub.
Además, la red del aula tiene **aislamiento de cliente**: los equipos no se ven
entre sí (medido desde f1-1, f2-8, f3-1 y f3-3: de ~80 hosts en la tabla ARP,
cada PC solo alcanza el gateway, a sí misma y algún vecino suelto, y solo por
el puerto 135/RPC; SMB/WinRM/RDP cerrados). No existe canal de ejecución
remota ni ruta de red para instalar sin tocar la máquina.

Por tanto, el primer arranque de cada PC es manual (o por tu herramienta de
despliegue del aula). A partir de ahí, todo es remoto.

## El comando (una sola línea, como administrador)

En cada PC nueva, abre PowerShell **como administrador** y pega:

```powershell
$env:ASTREA_TS_AUTHKEY='tskey-auth-PON_AQUI_TU_CLAVE'; $env:ASTREA_KIOSK_USER='Aluno(a)'; irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1 | iex
```

Eso: instala Tailscale y lo une al tailnet sin navegador (gracias a la
authkey), instala el agente como SYSTEM al arranque, registra el ayudante GUI
para cualquier administrador que inicie sesión, y deja a `Aluno(a)` como
administrador sin contraseña con autologon. Reinicia y la PC entra sola.

### La authkey de Tailscale (una vez)

Genérala en https://login.tailscale.com/admin/settings/keys :

- **Reusable**: ON  → la misma clave sirve para todas las PCs.
- **Ephemeral**: OFF → los nodos no desaparecen al apagar.
- Caducidad: la que quieras; úsala para toda la tanda y luego revócala.

Pega esa clave en `tskey-auth-...` del comando. Sin authkey, el instalador
abre el login de Tailscale en el navegador y hay que iniciar sesión a mano.

## Sin teclear nada: USB o carpeta compartida

Copia en una memoria USB (o carpeta de red) estos archivos de `scripts/`:

```
install.cmd
install.ps1
install.env            ← copia de install.env.example con tu authkey
astrea-agent.exe       ← opcional: instala sin descargar (aulas sin Internet)
```

En cada PC: doble clic en `install.cmd` → acepta el UAC → listo. El instalador
lee `install.env` (lo que ya esté en el entorno gana) y pasa **todas** las
variables `ASTREA_*` a la ventana de administrador, aunque el UAC pida otra
cuenta. `install.env` está en `.gitignore`: lleva la authkey, no lo subas.

## Desatendido como SYSTEM (GPO, Intune, PDQ)

`install.cmd` (o `install.ps1`) se puede lanzar como **script de arranque** de
GPO, script de plataforma de Intune o paso de PDQ, todos como SYSTEM:

- No hace falta nadie con sesión: el agente queda como SYSTEM al arranque.
- No pregunta nada ni hace `pause`; devuelve código 0/1.
- Log completo en `C:\ProgramData\Astrea\install.log`.
- Sin `ASTREA_TS_AUTHKEY` falla con un mensaje claro (no hay navegador).

## Qué hace como administrador

- Excluye `C:\ProgramData\Astrea` de Windows Defender antes de copiar el
  agente (lo borraba por no estar firmado). `ASTREA_NO_AV_EXCLUSION=1` lo evita.
- Modo aula: la cuenta tiene que existir (no la crea); la activa, la mete en
  Administradores (por SID, vale en inglés y portugués), contraseña en blanco
  por ADSI y autologon.
- Avisa si la cuenta que va a usar la PC no es administradora: el ayudante GUI
  (pantalla, ratón, teclado) solo arranca en sesiones de administradores.
- `config.json` y `hub-config.txt` quedan legibles solo por Administradores y
  SYSTEM.

## Variables relevantes

| Variable               | Para qué |
|------------------------|----------|
| `ASTREA_TS_AUTHKEY`    | unir al tailnet sin navegador (reusable) |
| `ASTREA_KIOSK_USER`    | cuenta a dejar admin sin contraseña + autologon (p. ej. `Aluno(a)`); tiene que existir. Vacío = no toca cuentas |
| `ASTREA_NO_AV_EXCLUSION=1` | no añadir la exclusión de Defender |
| `ASTREA_LOCAL_EXE`     | instalar este `astrea-agent.exe` sin descargar (automático si está junto al script) |
| `ASTREA_TOKEN`         | reutilizar un token concreto (por defecto genera/conserva) |
| `ASTREA_HEADLESS=0`    | modo clásico (una sola tarea al iniciar sesión) |

## Seguridad: qué implica el modo aula

`ASTREA_KIOSK_USER` deja una cuenta administradora **sin contraseña** con
autologon: cualquiera con acceso físico entra en esa sesión con permisos de
admin. Es un compromiso deliberado para equipos de aula bajo tu control, no
para máquinas con datos sensibles. Si no quieres eso en una PC concreta, omite
`ASTREA_KIOSK_USER` y el instalador no toca ninguna cuenta.

## Alternativas para no ir PC por PC

Si tienes muchas y no quieres repetir el paso físico:

1. **Herramienta de despliegue del aula** (GPO de Active Directory, Microsoft
   Intune, PDQ Deploy…): empuja el mismo comando como script de arranque. Es la
   vía estándar para un parque gestionado.
2. **Habilitar WinRM por GPO** y darme una credencial de administrador común:
   con eso sí puedo lanzar la instalación en lote desde el hub — pero requiere
   que el aislamiento de red permita al menos al hub/una PC llegar a las demás.

Sin una de esas, el primer toque de cada PC es inevitablemente local.

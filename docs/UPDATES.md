# Actualizaciones automáticas

Desde v0.3.0 toda la flota se mantiene en la última release de
[GitHub Releases](https://github.com/Solar2004/Astrea/releases) sin tocar nada.

| Pieza | Cómo se actualiza | Cuándo |
|-------|-------------------|--------|
| **Agente** (cada PC) | Solo: comprueba la release, descarga, verifica SHA-256 y se reinicia | ~3 min tras arrancar y luego cada ~6 h (con jitter) |
| **Hub + panel** (PC principal) | `astrea-update.timer` ejecuta `astrea-hub update` | Una vez al día |
| **Todo a la vez, ya** | `astrea-hub update` | A mano |

El agente **no se reinicia mientras hay un job o una descarga en curso**: lo
reintenta cada 20 min. Los servicios supervisados (llama-server, ComfyUI…)
vuelven solos tras el reinicio (~5 s sin conexión).

## Comandos

```sh
astrea-hub update --check     # versiones de hub, panel y cada PC, sin instalar
astrea-hub update             # hub + panel + todos los agentes
astrea-hub update --fleet     # solo agentes
astrea-hub update --local     # solo hub y panel de esta PC
astrea-hub update --force     # reinstalar aunque esté al día / con jobs en marcha
```

Desde la IA: `astrea.agent.update` (con `check_only`, `version`, `force`) y
`astrea.admin.status`, que ahora incluye `version`, `latest_version` y
`update_available`. Desde el panel: `POST /api/update?machine=NOMBRE|all|hub`.

## Configuración del agente

En `config.json` del agente:

```json
{ "auto_update": "auto" }
```

- `auto` (por defecto): instala solo cuando está libre.
- `notify`: solo comprueba; `admin.status` avisa de que hay versión nueva.
- `off`: nunca comprueba.

`ASTREA_UPDATE_URL` cambia el origen (espejo, o servidor local para pruebas):
debe servir `/latest` (redirección a `.../tag/vX.Y.Z`),
`/download/<tag>/<asset>` y `/download/<tag>/SHA256SUMS.txt`.

## Cómo funciona

1. `HEAD .../releases/latest` → la redirección dice el tag (sin la API de
   GitHub, así que sin límite de peticiones).
2. Descarga `SHA256SUMS.txt` y el binario de su plataforma a `<exe>.new`;
   si el hash no cuadra se descarta y no se toca nada.
3. Renombra el binario en uso a `<exe>.old` (Windows permite renombrar un
   exe en ejecución, no borrarlo) y pone el nuevo en su sitio.
4. Reinicio:
   - Linux con systemd: sale con código 75 y `Restart=always` arranca el nuevo.
   - Windows / sin supervisor: lanza el nuevo binario desacoplado y sale.
     En modo sin sesión, el agente pide antes al ayudante GUI que se
     reinicie también (`agent.restart`).
5. Al arrancar, el nuevo borra los `*.old` que ya no estén en uso.

Las builds de desarrollo (`version = dev`) nunca se auto-actualizan.

## Venir de v0.2.0 / v0.2.1 (una sola vez)

Los agentes v0.2.x no saben actualizarse solos, pero **no hace falta ir a cada
PC**: el hub los migra en remoto usando su `exec`.

**1. En tu PC principal** (actualiza hub + panel y activa el timer diario):

```sh
curl -fsSL https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.sh | bash -s hub
```

**2. Migra los agentes** (descarga el agente nuevo en cada PC, verifica el
SHA-256, lo cambia y lo reinicia; tarda ~10 s por PC):

```sh
astrea-hub update --migrate
```

O desde el panel: botón **Actualizar** en cada PC.

Las PCs apagadas se quedan como estaban: repite el comando cuando estén
encendidas. A partir de ahí, nunca más: se actualizan solas.

Si una migración falla, el agente viejo sigue funcionando (el binario solo se
cambia tras verificar el hash). Como último recurso, reinstala en esa PC con
el MSI o `install.ps1`, que conservan el token.

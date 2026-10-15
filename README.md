# Astrea

> Control total de una máquina remota **para tu agente de IA**. Como RustDesk,
> pero en vez de pantalla y mouse para un humano, expone **herramientas** para
> que tu IA (ej. Hermes) opere la máquina sola: ejecutar, inspeccionar,
> instalar, mover el mouse, capturar pantalla y **crear APIs internas en tiempo
> real** — todo sobre una red privada Tailscale, sin abrir puertos ni exponer
> dominio.

```
┌──────────────┐   MCP (stdio)   ┌──────────────┐   WebSocket + HTTP   ┌─────────────────┐
│  Tu agente   │ ───────────────▶│  astrea-hub  │ ═══ sobre tailnet ══▶ │  astrea-agent   │
│  (Hermes)    │   tools nativas │ (MCP server) │                      │ (PC controlada) │
└──────────────┘                 └──────────────┘                      └─────────────────┘
```

El **agente** corre en la PC a controlar (Windows o Linux). El **hub** es un
servidor MCP que tu agente carga; descubre las capacidades del agente y las
expone como herramientas (`astrea.exec`, `astrea.screen.capture`, …). Diseño
completo en [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

> ⚠️ Astrea le da a una IA control sin restricciones de la máquina. Léelo:
> [`SECURITY.md`](SECURITY.md). Úsalo solo sobre tu propio tailnet.

## Instalación

### 0. Red — Tailscale en todas las máquinas (una vez)

Instala [Tailscale](https://tailscale.com/download) en la PC principal y en la
PC a controlar, con la **misma cuenta**. Anota la IP `100.x.y.z` de cada una.

### 1. PC a controlar

**Windows — opción A, instalador MSI** (doble clic): descarga
`astrea-agent.msi` desde la [última release](https://github.com/Solar2004/Astrea/releases/latest)
y ábrelo. Hace todo lo de abajo y deja el bloque de config del hub en
`C:\ProgramData\Astrea\hub-config.txt`.

**Windows — opción B, un comando** (PowerShell como administrador):

```powershell
irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1 | iex
```

Cualquiera de las dos descarga el agente, escribe la config con un token nuevo,
lo registra para arrancar **al encender la PC, aunque nadie inicie sesión** (los
modelos vuelven solos tras un reinicio; pantalla/mouse/teclado se activan al
iniciar sesión, ver [`docs/HEADLESS.md`](docs/HEADLESS.md)), y deja el bloque de
config para el hub.

> ¿Algo falla? Ejecuta
> `& "$env:ProgramData\Astrea\astrea-agent.exe" --diagnose` y pásame la salida
> (no incluye el token). Detalle en [`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md).

**Linux**:

```sh
curl -fsSL https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.sh | bash -s agent
```

### 2. PC principal (donde corre tu agente)

```sh
curl -fsSL https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.sh | bash -s hub
```

Instala y conecta Tailscale si falta, baja el **hub** y el **panel**, deja el
panel corriendo solo en `http://127.0.0.1:7780` y crea `~/.config/astrea/`.

Registra cada PC que instales (con la IP y el token que te dio su instalador):

```sh
curl -fsSL https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.sh | bash -s add-machine pc-cuarto 100.x.y.z <TOKEN>
```

Y engancha el hub a Hermes una vez — en `~/.hermes/config.yaml` (o tu perfil),
bajo `mcp_servers`:

```yaml
  astrea:
    command: /home/TU_USUARIO/.local/bin/astrea-hub
    args: ["--config", "/home/TU_USUARIO/.config/astrea/machines.json"]
    enabled: true
```

Reinicia Hermes: aparecen las herramientas `astrea.*`. Con varias PCs, cada una
gana un argumento `machine` y `astrea.fleet.status` las resume (ver
[`docs/FLEET.md`](docs/FLEET.md)).

## Actualizaciones

Todo se actualiza solo: los agentes cada ~6 h (sin interrumpir jobs ni
descargas) y el hub y el panel una vez al día. Para hacerlo ya:
`astrea-hub update` (o `--check` para solo mirar). Detalle en
[`docs/UPDATES.md`](docs/UPDATES.md).

## Desde el código

```sh
make all        # agent + hub para esta máquina
make windows    # bin/astrea-agent.exe (cross-compile, sin CGo)
make test       # tests
```

## Capacidades

| Herramienta           | Qué hace                                                   |
|-----------------------|------------------------------------------------------------|
| `astrea.exec`         | Ejecuta un comando (cmd, powershell, bash, python…), salida en vivo |
| `astrea.fs.list/read/write` | Listar, leer y escribir archivos (texto o base64)    |
| `astrea.sys.info`     | OS, plataforma, uptime, memoria                            |
| `astrea.proc.list/kill` | Listar y matar procesos                                  |
| `astrea.mouse.move/click` | Mover y hacer clic con el mouse                        |
| `astrea.key.type`     | Escribir texto por teclado                                 |
| `astrea.screen.capture` | Captura de pantalla — la IA la recibe como imagen        |
| `astrea.service.start/stop/status/logs` | Servidores supervisados (ollama, llama-server, ComfyUI): reinicio, autoarranque, `expose` a `/dyn` |
| `astrea.job.start/status/logs/cancel` | Tareas largas en segundo plano (instalar torch, convertir modelos) |
| `astrea.deps.check/ensure` | Detecta e instala git, python, CUDA… (winget / apt / pacman) |
| `astrea.sys.idle`     | ¿Hay alguien usando la PC? Inactividad y ventana/juego en primer plano |
| `astrea.power.awake/wake/action` | No suspender mientras sirve modelos, Wake-on-LAN, bloquear/reiniciar/apagar |
| `astrea.task.define/list/get/delete/run` | Automatizaciones creadas por la IA; cada una es su propia tool `astrea.task.<name>` |
| `astrea.endpoint.create/list/delete` | APIs HTTP dinámicas en `/dyn/<name>` (exec/proxy/static/task) |
| `astrea.admin.lock/status/stop` | Kill switch y control del agente                 |
| `astrea.agent.update/restart` | Actualizar el agente a la última release (verificada) y reiniciarlo |
| `astrea.fleet.status` | (hub) Resumen de todas las PCs: online, CPU/RAM, GPUs con VRAM libre, quién está activo, servicios |

### Endpoints dinámicos — el diferenciador

Tu agente puede montar rutas HTTP en caliente sobre el tailnet:

- `kind: exec` — corre un comando; el body de la petición llega por stdin y el
  stdout es la respuesta.
- `kind: proxy` — reverse-proxy a un servicio local (ej. un modelo en
  `http://127.0.0.1:11434`), exponiéndolo como API interna.
- `kind: static` — respuesta fija.
- `kind: task` — publica una task (ver abajo) como API: body JSON o query string
  son sus argumentos.

Así Hermes puede, por ejemplo, instalar un modelo local con `exec`, levantarlo, y
crear un endpoint `proxy` para consumirlo — todo en tiempo real.

### Tasks — automatizaciones que la IA crea en caliente

La IA define en la PC remota una tarea con nombre y parámetros, y al instante
aparece como una tool nueva (`astrea.task.<name>`), sin reiniciar nada. Dos tipos:

- **script** — PowerShell/Python/bash/cmd/node con lógica propia ("¿qué modelos
  tengo instalados?", "encola este workflow en ComfyUI").
- **macro** — secuencia de capacidades ("abre Roblox, espera, captura la
  pantalla").

Persisten entre reinicios. Ejemplos completos en [`docs/TASKS.md`](docs/TASKS.md).

## Varias PCs — una flota

El hub maneja todas tus PCs a la vez. Crea `~/.config/astrea/machines.json`
(el instalador imprime el bloque de cada máquina):

```json
{"machines": [
  {"name": "pc-sala",  "ws": "ws://100.101.1.1:7700/ws", "token": "…"},
  {"name": "pc-cuarto","ws": "ws://100.101.1.2:7700/ws", "token": "…"}
]}
```

Con más de una máquina, cada herramienta gana un argumento `machine`, y
`astrea.fleet.status` da el panorama (GPU libre, quién está jugando) para que
la IA decida dónde correr cada cosa. Una sola PC funciona igual que antes, sin
`machine`. El panel web (`astrea-panel`) lee el mismo archivo.

### Gateway de modelos — todos los modelos locales como una sola API

```sh
astrea-hub gateway    # OpenAI-compatible en http://127.0.0.1:7788/v1
```

Cada servicio con `expose` (p.ej. llama-server o ollama en varias PCs) aparece
como un modelo `"<pc>/<endpoint>"`. Apunta cualquier cliente OpenAI al gateway
y elige la PC por el nombre del modelo:

```sh
curl http://127.0.0.1:7788/v1/models
curl http://127.0.0.1:7788/v1/chat/completions   -d '{"model": "pc-cuarto/llm", "messages": [{"role":"user","content":"hola"}]}'
```

Para servidores que exigen el nombre real del modelo (ollama), añádelo tras dos
puntos: `"model": "pc-cuarto/llm:qwen2.5:7b"`. Detalle en [`docs/FLEET.md`](docs/FLEET.md).

## Modos

`"mode"` en la config del agente:

- **`yolo`** (por defecto): la IA puede usar todas las capacidades.
- **`readonly`**: solo observar (archivos, stats, pantalla, logs, estado de
  servicios). Útil para PCs de otras personas.

`allow`/`deny` afinan cualquiera de los dos; `admin.lock` (kill switch) siempre
funciona. `"keep_awake": "auto"` (por defecto) impide que la PC se suspenda
mientras corre un servicio, job o descarga, así los modelos locales siguen disponibles.

## Seguridad

Token por máquina, allow/deny de métodos, kill switch (`admin.lock`), audit log
JSON de cada invocación, y cifrado WireGuard del tailnet. Detalle en
[`SECURITY.md`](SECURITY.md).

## Licencia

MIT — ver [`LICENSE`](LICENSE).

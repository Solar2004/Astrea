# Astrea — arquitectura

Astrea le da a Hermes (la IA en la PC principal Linux) **control total sobre una
PC remota** (Windows o Linux), enfocado a que la IA opere sola: ejecutar,
inspeccionar, instalar, levantar servicios de IA local y crear APIs internas en
tiempo real. Es "RustDesk para tu IA", pero en vez de pantalla+mouse para un
humano, expone **herramientas** para un agente.

## Piezas

```
┌──────────────┐   MCP (stdio)   ┌──────────────┐   WebSocket + HTTP   ┌─────────────────┐
│    HERMES    │ ───────────────▶│  astrea-hub  │ ═══ sobre tailnet ══▶ │  astrea-agent   │
│ (PC Linux)   │   tools nativas │ (MCP server) │                      │  (PC controlada)│
└──────────────┘                 └──────────────┘                      └─────────────────┘
```

- **astrea-agent**: daemon en la PC controlada. Un solo binario Go (cross-compila
  a Windows/Linux, sin runtime que instalar). Escucha en el tailnet y ejecuta
  *capabilities*. Hoy: `exec`, `fs.list/read/write`, `sys.info`, `proc.list/kill`.
- **astrea-hub**: servidor MCP (stdio) que Hermes carga. Al arrancar consulta las
  capabilities del agente y **genera una herramienta MCP por cada una**
  (`astrea.exec`, `astrea.fs.read`, …). Hermes las ve como tools nativas.
- **Tailnet (Tailscale)**: red overlay WireGuard que conecta las 3 PCs. Resuelve
  CGNAT / internet público sin abrir puertos ni exponer dominio. El agente se
  bindea a su IP `100.x.y.z` y el hub marca hacia ahí.

## Canal de control (protocolo)

Una conexión WebSocket de larga vida transporta tramas JSON (`internal/protocol`):

- `request`  hub → agente: `{id, method, payload}`
- `stream`   agente → hub: salida incremental (p.ej. stdout de `exec` en vivo)
- `result`   agente → hub: éxito terminal
- `error`    agente → hub: fallo terminal
- `cancel`   hub → agente: aborta la petición (el hub dejó de esperar)

Cada `request` recibe exactamente un `result`/`error`, precedido por los `stream`
que haga falta. El hub correlaciona por `id` (ver `internal/transport/client.go`).

## Seguridad

Esto es, por diseño, **ejecución remota de código total** sobre tu máquina para tu
IA. Capas actuales y pendientes:

- **Transporte**: cifrado por WireGuard (tailnet). El conjunto de peers ya está
  restringido a tus dispositivos.
- **Token bearer** (`ASTREA_TOKEN`): segundo factor por si el agente queda
  bindeado fuera del tailnet por error. Comparación en tiempo constante.
- **Pendiente (fases siguientes)**: log de auditoría de cada invocación, kill
  switch, allowlist de capabilities por sesión, mTLS opcional.

## Mapa de fases

- **F0** Red: Tailscale en las 3 PCs. *(infra, lo hace el usuario)*
- **F1** Núcleo: agent + hub + exec/fs/sys. **✓ hecho**
- **F2** Input/pantalla: mouse, teclado, captura. **✓ hecho** (Windows vía Win32
  SendInput sin CGo; Linux X11 vía xdotool).
- **F3** Endpoints dinámicos: `endpoint.create` monta rutas HTTP en caliente
  (`exec`/`proxy`/`static`) → APIs internas en tiempo real. **✓ hecho**
- **Seguridad**: token, allow/deny, kill switch (`admin.lock`), audit log,
  reconexión del hub, caché de capabilities. **✓ hecho**
- **Tasks**: automatizaciones definidas por la IA (`task.define`, kinds
  `script`/`macro`), cada una publicada como tool MCP en vivo y opcionalmente
  como `/dyn/<name>`. **✓ hecho** — ver [`TASKS.md`](TASKS.md).
- **Flota**: el hub lee `machines.json` y expone todas las PCs con un argumento
  `machine` por tool; `astrea.fleet.status` resume la flota. `astrea-hub gateway`
  da una API OpenAI única sobre los modelos expuestos. **✓ hecho** — ver
  [`FLEET.md`](FLEET.md).
- **F4** *(siguiente)* Servicios/IA: `service.spawn` para procesos de larga vida
  (ollama/llama.cpp) con seguimiento, y helpers de instalación de paquetes.
  Hoy ya es posible vía `exec` + `endpoint.create` kind=proxy.

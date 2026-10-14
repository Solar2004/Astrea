# Flota — varias PCs desde un solo hub

El hub (`astrea-hub`) ya no habla con una sola máquina: lee una lista y expone
las capacidades de todas. El gateway (`astrea-hub gateway`) pone una única API
OpenAI delante de los modelos locales de todas ellas.

## machines.json

Ruta por defecto: `~/.config/astrea/machines.json` (en Windows,
`%AppData%\astrea\machines.json`). Mismo formato que el `panel.json` del panel;
si no existe `machines.json`, el hub lee `panel.json` del mismo directorio.

```json
{"machines": [
  {"name": "pc-sala",   "ws": "ws://100.101.1.1:7700/ws", "token": "TOKEN_A"},
  {"name": "pc-cuarto", "ws": "ws://100.101.1.2:7700/ws", "token": "TOKEN_B"}
]}
```

- `name`: identificador corto (letras, dígitos, `_`, `-`); es el prefijo de
  modelo en el gateway y el valor del argumento `machine`.
- `ws`: URL del control-channel del agente (`ws://IP-tailnet:7700/ws`).
- `token`: el token de esa máquina.

Un hub sin `machines.json` y sin `--ws` asume el agente local
`ws://127.0.0.1:7700/ws` (como antes). `--ws`/`--token` fuerzan una sola
máquina e ignoran el archivo.

## Cómo se ven las tools

- **Una sola máquina**: las tools son idénticas a antes (`astrea.exec`, …),
  sin argumento extra.
- **Varias máquinas**: cada tool gana una propiedad `machine`. Es obligatoria
  cuando la capability existe en más de una PC; si solo una la tiene (p.ej. una
  task definida en una máquina concreta), se enruta sola.
- `astrea.fleet.status` (lo sirve el hub, no un agente) devuelve, por máquina:
  online, OS, modo, CPU/RAM, GPUs con VRAM libre, `user_active` + proceso en
  primer plano, y los servicios. Es lo que la IA mira para decidir dónde correr
  algo pesado sin molestar a quien esté usando la PC.

Las capacidades se cachean por máquina en disco, así que las tools aparecen al
instante aunque una PC esté apagada; el hub resincroniza cada 30 s y publica
`tools/list_changed` cuando algo cambia (una PC vuelve, la IA define una task…).

## Gateway de modelos

```sh
astrea-hub gateway --listen 127.0.0.1:7788
```

Descubre cada servicio con `expose` (ver [`TASKS.md`](TASKS.md) y `service.start`)
y lo publica como modelo `"<máquina>/<endpoint>"`:

```
GET  /v1/models                         → lista los modelos de toda la flota
POST /v1/chat/completions               → enruta por el campo "model"
POST /v1/completions, /v1/embeddings    → igual
```

Selección del modelo (campo `"model"`):

| Valor                         | Va a                                             |
|-------------------------------|--------------------------------------------------|
| `pc-cuarto/llm`               | servicio `llm` de `pc-cuarto`                    |
| `pc-cuarto/llm:qwen2.5:7b`    | igual, pero pide `qwen2.5:7b` al servidor local  |
| `llm`                         | si solo una PC expone `llm`; si no, error claro  |

El gateway reescribe el `"model"` a lo que está tras los dos puntos antes de
reenviar. Para **llama-server** el campo da igual (sirve el modelo cargado); para
**ollama** hay que dar el nombre real con `:`. El streaming SSE se reenvía tal cual.

El gateway solo escucha en localhost. No añade autenticación: es para que tu PC
principal y Hermes usen los modelos de la flota; no lo expongas fuera.

## Prueba manual rápida

1. En una PC: `service.start` con `expose {endpoint: "llm", port: 8080}` sobre un
   llama-server/ollama ya levantado.
2. `curl http://127.0.0.1:7788/v1/models` muestra `<pc>/llm`.
3. `curl .../v1/chat/completions -d '{"model":"<pc>/llm","messages":[...]}'`
   responde desde esa PC.
4. Apaga esa PC: el modelo desaparece de `/v1/models` en el siguiente refresco.

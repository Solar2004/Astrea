# Visión local — `vision.locate` (manos finas, capa 3)

Astrea controla el escritorio en tres capas, de barata a cara. Usa siempre la
más barata que funcione:

1. **`browser.*`** — web por DOM. Cero GPU, texto exacto.
2. **`ui.snapshot` / `ui.find` / `ui.act`** — árbol de UI Automation de Windows.
   Cero GPU, instantáneo, cubre casi toda app nativa (Win32/WinUI/Office).
3. **`vision.locate`** — un modelo de visión local en la GPU de la PC. Solo para
   lo que las capas 1 y 2 no ven: juegos, lienzos, apps dibujadas a mano.

El modelo solo **ve** (devuelve coordenadas); quien **decide** sigue siendo Hermes.

## Modelo recomendado

**UI-TARS-1.5-7B** (ByteDance). Es el mejor open-source en ScreenSpot-Pro (61.6)
y cabe en 16 GB (FP16 ~14 GB; Q8 ~8 GB; Q4 ~6 GB, dejando sitio a tus otros
servicios). Al estar basado en Qwen2.5-VL se sirve con vLLM estándar.

Alternativa: **GUI-Actor-7B** (ScreenSpot-Pro 44.6, supera a UI-TARS-72B), pero su
cabeza de atención propia no corre en llama.cpp — necesita vLLM/transformers.

## Cómo servir el modelo

### Windows (las PCs del aula: RTX 2000 Ada 16 GB) → llama.cpp

vLLM es Linux‑first y cuesta en Windows; la vía práctica es **llama.cpp**
(`llama-server`, release CUDA), que sirve el mismo endpoint OpenAI‑compatible y
acepta imágenes con un GGUF + `mmproj`. UI‑TARS‑1.5‑7B tiene GGUF listo:

1. Bajar el binario de llama.cpp (release `...-bin-win-cuda-...zip`, trae las
   DLLs de cuBLAS) y descomprimir, p. ej. en `C:\Astrea\llama`.
2. Bajar el modelo (~4.7 GB) y su proyector de visión (~1 GB) con `net.download`:
   - `hf://mradermacher/UI-TARS-1.5-7B-GGUF/UI-TARS-1.5-7B.Q4_K_M.gguf`
   - `hf://mradermacher/UI-TARS-1.5-7B-GGUF/UI-TARS-1.5-7B.mmproj-Q8_0.gguf`
3. `vision.serve` = arrancar `llama-server` con el modelo en la GPU:

```json
{
  "vision": {
    "endpoint": "http://127.0.0.1:8100/v1/chat/completions",
    "model": "ui-tars",
    "serve": ["C:\\Astrea\\llama\\llama-server.exe",
              "-m", "C:\\Astrea\\models\\UI-TARS-1.5-7B.Q4_K_M.gguf",
              "--mmproj", "C:\\Astrea\\models\\UI-TARS-1.5-7B.mmproj-Q8_0.gguf",
              "-ngl", "99", "--host", "127.0.0.1", "--port", "8100"],
    "idle_stop_sec": 300,
    "coords": "pixel"
  }
}
```

`-ngl 99` descarga todas las capas a la GPU. Q4_K_M + mmproj caben de sobra en
16 GB (ocupa ~13 GB de VRAM mientras corre; latencia ~2‑3 s por consulta en una
RTX 2000 Ada).

**`coords: "pixel"`** para el GGUF de llama.cpp: este checkpoint devuelve las
coordenadas en píxeles de la imagen que se le manda (verificado en real), no
normalizadas 0‑1000. En vLLM el modelo oficial responde 0‑1000 → usa
`"coords": "norm1000"` (el valor por defecto) en ese caso.

### Linux → vLLM

```json
"serve": ["python", "-m", "vllm.entrypoints.openai.api_server",
          "--model", "ByteDance-Seed/UI-TARS-1.5-7B", "--port", "8100"]
```

## Formato de coordenadas

UI‑TARS no responde en píxeles: usa su plantilla de *grounding* y devuelve
`Action: click(point='<point>x y</point>')` con coordenadas **normalizadas
0‑1000**. `vision.locate` ya manda esa plantilla y convierte a pantalla
(`coords: "norm1000"`, por defecto). Un modelo que devuelva píxeles de la imagen
(algunos Qwen2.5‑VL) se configura con `coords: "pixel"`.

## Auto‑instalación (un interruptor) — `vision.setup`

Lo más fácil: no bajes ni configures nada a mano. Llama una vez:

```jsonc
vision.setup { "on": true }
```

El agente, en cuanto tenga internet, **descarga solo** el modelo (y en Windows
el runtime llama.cpp CUDA) en segundo plano, reanudable, reintentando hasta
terminar, y luego **autoconfigura** `serve`/`endpoint`/`coords`. A partir de ahí
`vision.locate` funciona. El interruptor se recuerda entre reinicios, así que
una descarga a medias continúa sola. `vision.setup` sin argumentos (o
`vision.status`) muestra el progreso (MB bajados, fase, `ready`).

Para activarlo de fábrica al actualizar, pon `"auto_provision": true` dentro de
`vision` en la config. URLs del modelo configurables con `model_url`/`mmproj_url`.

## Config: arrancar el modelo bajo demanda

El modelo **no corre por defecto** (ocupa GPU). Astrea lo arranca cuando la IA
lo pide, trabaja, y lo apaga cuando lleva un rato sin usarse. Para eso, `serve`
dice cómo lanzarlo:

```json
{
  "vision": {
    "endpoint": "http://127.0.0.1:8100/v1/chat/completions",
    "model": "ByteDance-Seed/UI-TARS-1.5-7B",
    "serve": ["python", "-m", "vllm.entrypoints.openai.api_server",
              "--model", "ByteDance-Seed/UI-TARS-1.5-7B", "--port", "8100"],
    "idle_stop_sec": 300
  }
}
```

- Sin `endpoint`/`model`, `vision.locate` responde `unavailable`.
- Sin `serve`, Astrea no lo lanza él mismo: solo reutiliza un servidor que ya
  esté corriendo (lo levantas tú con `service.start`).
- `idle_stop_sec` (def. 300): tras ese tiempo sin usarse, Astrea lo apaga y
  libera la GPU.

## Ciclo de vida (encender solo cuando se usa)

- `vision.start` — lo enciende y **espera a que cargue** (la primera vez, los
  pesos → GPU tardan ~30-90 s). Úsalo antes de una tanda de trabajo.
- `vision.locate` — si no está encendido, lo enciende solo (espera la carga).
- `vision.stop` — lo apaga ahora y libera la GPU.
- `vision.status` — dice si está configurado, encendido y listo, su `uptime`,
  cuánto lleva sin usarse, la **RAM del proceso** (`ram_mb`) y la **VRAM** en uso
  (`gpu`, de `nvidia-smi`). Úsalo para ver el consumo.

Astrea en sí (el agente) consume muy poco: ~20 MB de RAM y ~0 % de CPU en reposo.
El VLM es lo pesado, y solo mientras está encendido:

| Precisión | VRAM aprox. (UI-TARS-1.5-7B) |
|---|---|
| Q4 | ~6 GB |
| Q8 | ~8 GB |
| FP16 | ~14 GB |

## Uso

```jsonc
vision.locate { "instruction": "el botón rojo de Entregar", "click": true }
// → { "x": 812, "y": 436, "clicked": true }
```

`vision.locate` toma la captura por el mismo `screen.capture` (se reenvía al
escritorio y desbloquea la PC si hace falta), la manda al modelo, convierte el
píxel de la imagen a coordenada de pantalla con `origin`/`scale`, y opcionalmente
hace clic ahí con `mouse.*`.

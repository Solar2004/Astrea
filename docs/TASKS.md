# Tasks — automatizaciones creadas por la IA

Una *task* es una unidad de trabajo con nombre y parámetros que la IA define en
la PC controlada (`astrea.task.define`). Al definirla:

1. Se guarda en `<data_dir>/tasks.json` (sobrevive a reinicios del agente).
2. Se registra como capability `task.<name>` en el agente.
3. El hub la publica como tool MCP `astrea.task.<name>` y avisa al cliente
   (`tools/list_changed`). Si el cliente no refresca, `astrea.task.run` sirve
   igual.
4. Opcional: `astrea.endpoint.create {kind: "task"}` la expone como API HTTP en
   `/dyn/<name>` para que otros programas del tailnet la consuman.

`task.get` devuelve la definición completa para que la IA la corrija;
`task.define` con el mismo nombre la reemplaza.

## Tipo `script`

El script se ejecuta con el `shell` elegido (`powershell`, `pwsh`, `cmd`,
`bash`, `sh`, `python`, `node`). Recibe los argumentos:

- como variables `ASTREA_ARG_<NOMBRE>` (en mayúsculas; los no-string van en JSON),
- todos juntos en JSON en `ASTREA_ARGS` y por stdin.

stdout/stderr vuelven en vivo. Para devolver datos estructurados, el script
escribe JSON en el archivo `$ASTREA_RESULT`. Si ese JSON trae
`"images": [{"mime_type": "image/png", "data": "<base64>"}]`, la IA las ve como
imágenes. Los `default` y `required` del schema de `params` se aplican.

### Hora actual

```json
{
  "name": "hora", "kind": "script", "shell": "powershell",
  "description": "Fecha y hora actual de esta PC y su zona horaria",
  "script": "@{ now = (Get-Date).ToString('o'); tz = (Get-TimeZone).Id } | ConvertTo-Json | Set-Content -Encoding utf8 $env:ASTREA_RESULT"
}
```

### Modelos locales instalados / descargar uno

```json
{
  "name": "modelos", "kind": "script", "shell": "powershell",
  "description": "Lista los modelos de Ollama instalados; con 'pull' descarga ese modelo",
  "params": {"type": "object", "properties": {"pull": {"type": "string", "description": "Modelo a descargar, ej. qwen2.5:7b"}}},
  "timeout_sec": 3600,
  "script": "if ($env:ASTREA_ARG_PULL) { ollama pull $env:ASTREA_ARG_PULL }\n$r = Invoke-RestMethod http://127.0.0.1:11434/api/tags\n$r.models | Select-Object name, size, modified_at | ConvertTo-Json | Set-Content -Encoding utf8 $env:ASTREA_RESULT"
}
```

### Encolar un workflow de ComfyUI

```json
{
  "name": "comfy_generar", "kind": "script", "shell": "python",
  "description": "Genera una imagen en ComfyUI con un prompt y la devuelve",
  "params": {"type": "object", "properties": {"prompt": {"type": "string"}, "workflow": {"type": "string", "default": "C:/ComfyUI/workflows/api_txt2img.json"}}, "required": ["prompt"]},
  "timeout_sec": 600,
  "script": "import json, os, time, base64, urllib.request as u\nA = json.loads(os.environ['ASTREA_ARGS'])\nwf = json.load(open(A['workflow']))\nwf['6']['inputs']['text'] = A['prompt']  # nodo CLIPTextEncode del workflow\nreq = u.Request('http://127.0.0.1:8188/prompt', json.dumps({'prompt': wf}).encode(), {'Content-Type': 'application/json'})\npid = json.load(u.urlopen(req))['prompt_id']\nwhile True:\n    h = json.load(u.urlopen(f'http://127.0.0.1:8188/history/{pid}'))\n    if pid in h: break\n    time.sleep(1)\nimgs = []\nfor node in h[pid]['outputs'].values():\n    for im in node.get('images', []):\n        q = f\"filename={im['filename']}&subfolder={im['subfolder']}&type={im['type']}\"\n        imgs.append({'mime_type': 'image/png', 'data': base64.b64encode(u.urlopen('http://127.0.0.1:8188/view?' + q).read()).decode()})\njson.dump({'prompt_id': pid, 'images': imgs}, open(os.environ['ASTREA_RESULT'], 'w'))"
}
```

## Tipo `macro`

`steps` se ejecutan en orden; cada uno llama a cualquier capability (`exec`,
`mouse.move`, `mouse.click`, `key.type`, `screen.capture`, otra `task.x`…) o al
built-in `sleep` (`{"ms": 1500}` o `{"sec": 2}`). `"{{param}}"` en los args se
reemplaza por el argumento de la task (si el string es solo `"{{x}}"`, conserva
el tipo: números siguen siendo números). Un paso que falla detiene la macro,
salvo `"continue_on_error": true`. Todas las imágenes capturadas se devuelven
juntas.

### Abrir Roblox y ver qué juegos hay

```json
{
  "name": "roblox_juegos", "kind": "macro",
  "description": "Abre Roblox, espera a que cargue y devuelve una captura de la pantalla de inicio",
  "params": {"type": "object", "properties": {"espera_ms": {"type": "integer", "default": 15000}}},
  "steps": [
    {"call": "exec", "args": {"shell": "powershell", "command": "Start-Process 'roblox://'"}},
    {"call": "sleep", "args": {"ms": "{{espera_ms}}"}},
    {"call": "screen.capture", "args": {"max_width": 1568}}
  ]
}
```

La IA mira la imagen y, si hace falta, sigue con `mouse.move`/`mouse.click`
usando `origin` y `scale` de la captura para convertir coordenadas.

## Seguridad

- Cada paso de una macro pasa por el guard (allow/deny + kill switch) y queda
  en el audit log, igual que una llamada directa. `admin.lock` frena también
  las tasks.
- Las allow/deny lists aceptan comodines: `"allow": ["task.*", "screen.capture"]`.
- Las tasks anidadas tienen un límite de profundidad (8).
- `/dyn/<name>` de tipo `task` **no** pide token (como el resto de `/dyn`):
  cualquiera en tu tailnet puede ejecutar esa task. Publica solo las que te
  parezca bien exponer.

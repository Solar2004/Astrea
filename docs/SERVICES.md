# Servicios por receta — diseño

Estado: **fase 1 hecha** (2026-10-25, sin publicar; probada en f3-c2). Ver §10.

## 1. Qué queremos

Un servicio que funciona en una PC (ComfyUI con sus modelos, UI-TARS, una API
propia…) tiene que poder pasar a otra PC **entero y funcionando con un clic**,
sin que nadie copie carpetas a mano ni se acuerde de qué script lanzar. Y cada
servicio publica un endpoint que se puede usar desde fuera (una web, un sistema
automático) pidiéndole al panel la lista de endpoints disponibles.

La pieza central es la **receta**: un archivo pequeño (KB) que describe todo lo
que hace falta para tener el servicio. **Crear un servicio = escribir su
receta.** Los bytes grandes (modelos de 20 GB) nunca van en la receta: la receta
dice qué archivos son (ruta, tamaño, sha256) y de dónde se pueden sacar.

```
receta (KB)  ── dónde vive ──►  panel (copia de seguridad, catálogo local)
                                astrea-services (repo git: las oficiales)
bytes (GB)   ── dónde viven ─►  las PCs que ya los tienen (copia entre PCs)
                                espejos de internet (HuggingFace, ModelScope…)
```

## 2. Estado actual (medido el 2026-10-25)

- Tres tipos de "servicio" que no se conocen entre sí: el obligatorio UI-TARS
  (escrito en Go, `internal/capability/visionprov.go`), los supervisados de
  `service.start` (por PC, `services.json`) y los endpoints `/dyn/` (por PC,
  `endpoints.json`). Ninguno sabe instalarse en otra PC.
- `comfyui` y `lorian-share` están en 12 PCs porque los lanzó
  `lorian-studio/farm/fleet.py`. El laptop no los tiene.
- Los modelos de ComfyUI difieren: f1-1 tiene 95 GB (incluye los de música),
  f1-6/7/8 y f2-8 tienen 84 GB, f3-1…4 tienen `.part` a medias, y f3-c1/c2/c3 solo
  39 GB. Parte de la diferencia es a propósito (`bootstrap.ps1 -MaxPriority`) y
  parte son descargas que se cortaron.
- lorian-studio ya resolvió a mano casi todo lo que una receta necesita:
  `manifest/models.json` (ruta, tamaño, sha256, prioridad, espejos),
  `bootstrap.ps1` (pasos idempotentes), `share.py` (servidor de archivos con
  Range) y `pdl.ps1` (descarga en paralelo de varias fuentes). **Esto se diseña
  para absorber eso dentro de Astrea**, no para inventarlo de cero.

## 3. La receta

Formato JSON (como el resto de Astrea; la IA lo escribe sin problema y no añade
dependencias). Un ejemplo recortado:

```json
{
  "schema": "astrea.recipe/v1",
  "name": "comfyui-minimax",
  "version": "1.0.0",
  "title": "ComfyUI · MiniMax H3",
  "description": "Render de vídeo de la granja Lorian.",
  "requires": { "os": "windows", "gpu": "nvidia", "vram_mb": 15000 },
  "root": "C:\\lorian",
  "profiles": {
    "basico":   { "groups": ["runtime", "p1"] },
    "completo": { "groups": ["runtime", "p1", "p2", "p3"], "default": true }
  },
  "artifacts": [
    { "id": "comfyui", "group": "runtime", "path": "dist/ComfyUI_windows_portable_nvidia.7z",
      "size": 2018444114, "urls": ["https://github.com/…/ComfyUI_windows_portable_nvidia.7z"],
      "unpack": { "to": "", "creates": "ComfyUI_windows_portable/python_embeded/python.exe" } },
    { "id": "vae", "group": "p1", "path": "models/vae/minimax_h3_video_vae_int8_convrot.safetensors",
      "size": 2811065184, "sha256": "52a2…", "urls": ["https://huggingface.co/…"] }
  ],
  "files": [
    { "path": "ComfyUI_windows_portable/ComfyUI/extra_model_paths.yaml", "content": "lorian:\n  base_path: …" }
  ],
  "setup": [
    { "name": "firewall 8188", "shell": "powershell",
      "unless": "Get-NetFirewallRule -DisplayName 'Lorian 8188' -EA SilentlyContinue",
      "run": "New-NetFirewallRule -DisplayName 'Lorian 8188' …" }
  ],
  "services": [
    { "name": "comfyui", "command": "${root}\\ComfyUI_windows_portable\\python_embeded\\python.exe -s …",
      "cwd": "${root}\\ComfyUI_windows_portable",
      "ready": { "url": "http://127.0.0.1:8188/system_stats" },
      "expose": { "endpoint": "comfyui", "port": 8188, "api": "comfyui", "auth": "key" } }
  ],
  "resources": { "gpu_mb": 14500, "gpu_exclusive": true },
  "global": false
}
```

Campos:

| Campo | Para qué |
|---|---|
| `name`, `version` | Identidad. Instalar otra versión = aplicar la diferencia. |
| `requires` | Qué PCs pueden tenerlo. Las que no cumplen salen como "No aplica" (igual que hoy UI-TARS sin NVIDIA). |
| `root` | Carpeta base. En comandos y rutas se escribe `${root}`; nada de rutas absolutas repetidas. |
| `profiles` | Subconjuntos de artefactos ("básico", "completo", "con música"). Sustituye a `-MaxPriority`. |
| `artifacts` | Archivos grandes: ruta relativa a `root`, tamaño, sha256, espejos. `unpack` los descomprime al llegar; `unpack.creates` (obligatorio) es una ruta que existe cuando ya están descomprimidos, y entonces el archivo comprimido no hace falta (así adoptar una PC no vuelve a bajarlo). Sin `urls` = solo se puede sacar de otra PC (ver puntos flacos). |
| `files` | Archivos pequeños escritos tal cual (configs). |
| `setup` | Pasos idempotentes. `unless` (o `creates: <ruta>`) decide si hace falta repetirlos; uno de los dos es obligatorio. |
| `services` | Lo que hoy es `service.start`: comando, entorno, sondeo `ready`, `expose`. |
| `expose.api` | Qué tipo de API es: `openai`, `comfyui` o `http` (con `docs` opcional). Así una web sabe cómo hablarle. |
| `expose.auth` | `none` (solo tailnet) o `key` (pide API key del panel). |
| `resources` | VRAM que ocupa y si necesita la GPU para él solo. |
| `global` | Si es `true`, va en **todas** las PCs que cumplan `requires`, también en las que lleguen nuevas. |

Las recetas con secretos (token de HuggingFace, clave de una API) **no los
llevan dentro**: ponen `"${secret:hf_token}"` y el panel lo rellena al instalar.

## 4. Flujos

### Instalar una receta en una PC (un clic en el panel)

```
panel ── recipe.apply {receta, perfil, fuentes} ──► agente destino
agente: 1. comprueba requires y disco libre
        2. plan: compara artifacts con su disco (tamaño, luego sha256)
           → solo baja lo que falta; retoma los .part
        3. baja de las PCs que lo tienen (varias a la vez, por trozos);
           si ninguna lo tiene, de los espejos
        4. descomprime, escribe files, corre setup (saltando lo hecho)
        5. crea/actualiza los services y sus endpoints
        6. guarda en installed.json: receta, versión, perfil, estado
panel ◄── progreso (fase, %, bytes, ETA) ── recipe.status
```

Es un trabajo de fondo con estado propio, como hoy el `Provisioner` de
UI-TARS: si el agente se reinicia o se corta la red, sigue donde iba.

### Adoptar lo que ya está instalado

Las 12 PCs ya tienen ComfyUI. Aplicarles la receta **no debe volver a bajar
nada**: el plan reconoce los archivos por tamaño y sha256 y los da por buenos,
reescribe lo pequeño si difiere y deja el servicio gestionado por la receta.
Así se migra sin tocar nada a mano (y `fleet.py services` deja de hacer falta).

### Capturar una receta desde una PC que funciona

`recipe.capture {service, dirs}` en la PC origen genera un borrador: toma el
comando, entorno y endpoint del servicio supervisado, recorre las carpetas que
le digas, mide y hashea los archivos grandes, y propone las carpetas de miles
de archivos pequeños (un Python embebido, nodos custom) como **un artefacto
empaquetado** (`pack`: el agente las comprime una vez y el zip pasa a ser un
artefacto más). El borrador se revisa en el panel y se guarda en el catálogo.

### Global

Una receta `global: true` la aplica el panel solo, en cada PC que cumpla
`requires`, cuando la PC aparece o cuando sale una versión nueva. UI-TARS pasa a
ser la primera receta global, en lugar de estar escrito en Go.

## 5. Dónde vive cada cosa

| Qué | Dónde | Por qué |
|---|---|---|
| Catálogo local de recetas | Panel: `~/.config/astrea/recipes/<name>/<version>.json` | El panel es el proceso que siempre está encendido y tiene la web. El hub **no**: es un MCP por stdio que arranca Hermes, no puede guardar ni servir nada. |
| Recetas oficiales | Repo `astrea-services` (`recipes/<name>/recipe.json` + README + scripts) | Para que no se pierdan y para tener "servicios por defecto". El panel las importa y puede publicar las propias. Solo texto: nunca modelos. |
| Instaladas en cada PC | Agente: `installed.json` + los `services.json` / `endpoints.json` de siempre | Cada agente sabe qué tiene aunque el panel esté apagado. |
| Bytes | Las propias PCs (cada agente sirve los artefactos de sus recetas) y los espejos | Copiar por la LAN es mucho más rápido que internet; cuantas más PCs lo tienen, más fuentes. |
| API keys | Panel: `keys.json` (solo el hash de cada clave, con sus permisos) | Se crean y se borran desde el panel. |

## 6. API para usar los servicios

Todo en el panel (puerto 7780), bajo `/api/v1/` para poder versionarla:

```
GET  /api/v1/endpoints               lista de endpoints usables
GET  /api/v1/recipes                 catálogo, con la matriz receta × PC
POST /api/v1/recipes/{name}/install  {machines, profile}
ANY  /svc/{endpoint}/…               pasarela: elige una PC libre que lo tenga
```

Un elemento de `/api/v1/endpoints`:

```json
{
  "endpoint": "comfyui", "recipe": "comfyui-minimax", "version": "1.0.0",
  "api": "comfyui", "auth": "key",
  "url": "http://<panel>:7780/svc/comfyui",
  "machines": [
    { "machine": "f3-1", "state": "running", "busy": false,
      "direct": "http://100.x.y.z:7700/dyn/comfyui" }
  ]
}
```

- `url` es la puerta buena: el panel comprueba la API key (si `auth: key`) y
  reparte entre las PCs que lo tienen funcionando. Para `api: openai` absorbe lo
  que hoy hace `astrea-hub gateway`.
- `direct` es para quien quiera elegir PC a mano (dentro del tailnet).
- API keys: se crean en el panel con un nombre y los endpoints a los que dan
  acceso (o `*`). Se manda `Authorization: Bearer <clave>`. Se pueden revocar.
- La IA (Hermes) recibe tools `recipe.*` y `fleet.endpoints` en el hub, que por
  dentro llaman a esta API del panel.

## 7. Puntos flacos y cómo se cubren

1. **El hub no es un servidor.** El "hub" en el que se guardan las recetas es el
   panel. Si el panel está caído, nadie puede instalar recetas nuevas, pero lo
   ya instalado sigue funcionando (cada agente tiene su `installed.json`).
2. **Archivos de una sola copia.** Un artefacto sin espejo que solo está en una
   PC se pierde si esa PC muere (el kit de lorian, un modelo entrenado por
   nosotros). El panel debe marcarlos en rojo ("una sola copia") y ofrecer
   copiarlos a un **depósito** (una carpeta en la PC Linux o un disco) que actúa
   como una fuente más.
3. **Carpetas con miles de archivos.** Listar en la receta cada archivo de un
   Python embebido es inmanejable. Se empaquetan como un artefacto (`pack`).
   Contrapartida: un cambio pequeño obliga a volver a bajar el paquete entero.
4. **Hashear 100 GB tarda.** Unos minutos por PC. El agente guarda el hash junto
   al tamaño y la fecha de modificación y solo lo recalcula si cambian.
5. **El descargador actual es de una sola fuente y una conexión**
   (`internal/download`). Para la copia entre PCs hay que portar a Go lo que hace
   `pdl.ps1`: trozos en paralelo desde varias fuentes, saltándose las que dan 404.
   Y saber qué PCs comparten LAN (hoy está escrito a mano en `fleet.py`): pasa
   a ser un campo `lan` en `machines.json`, porque por el tailnet con relé DERP
   va mucho más lento.
6. **La GPU se comparte mal.** Hoy ComfyUI ocupa 14 GB de 16 en cada PC del
   aula y UI-TARS necesita unos 5,3 GB. Con `resources` el agente sabe cuánto
   pide cada servicio; los marcados `gpu_exclusive` se paran cuando otro lo
   necesita (UI-TARS ya arranca y se para solo). Sin esto, "global" no sirve
   de mucho en esas PCs.
7. **`setup` es código arbitrario.** No es declarativo de verdad: un paso mal
   escrito que no es idempotente rompe la reinstalación. Mitigación: `unless` /
   `creates` obligatorios, cada paso guarda su resultado, y el registro de la
   instalación se ve en el panel.
8. **Una receta es ejecución remota como administrador en toda la flota.**
   Importar del repo oficial = confiar en lo que haya en ese commit. El panel
   fija cada receta importada a un commit y enseña la diferencia antes de
   actualizar. Nada se aplica solo desde el repo sin pasar por el panel.
9. **Puertos que chocan.** Dos recetas con el mismo puerto en la misma PC: el
   plan lo detecta antes de instalar y se niega.
10. **Dos sistemas mandando sobre lo mismo.** Mientras `fleet.py` siga lanzando
    `service.start`, pisa lo que haga la receta. Al adoptar ComfyUI hay que
    retirar `fleet.py services` (y `lorian-share` sobra: el agente ya sirve los
    archivos).
11. **Seguridad de la puerta directa.** Una API key en el panel no protege nada
    si el servicio también escucha en `0.0.0.0` (hoy ComfyUI lo hace en el
    8188) o si `/dyn/` sigue sin token. Para los servicios `auth: key`: el
    servicio escucha en `127.0.0.1`, se publica solo por `/dyn/`, y el agente
    exige en esa ruta el token de la flota, que solo tiene el panel.
12. **Versiones y marcha atrás.** Actualizar es aplicar la diferencia entre
    versiones. Volver atrás exige no borrar los artefactos viejos hasta que la
    nueva versión esté `running`: el borrado es un paso aparte ("limpiar").
13. **Operaciones largas.** Una instalación de 60 GB no cabe en una llamada con
    timeout. `recipe.apply` vuelve enseguida con un id y el progreso se consulta
    con `recipe.status` (como hoy `download.*` y `job.*`).

## 8. Estructura del código

```
internal/recipe/            # nuevo, sin dependencias del agente
  recipe.go                 # tipos, Validate, expansión de ${root}/${secret:…}
  plan.go                   # disco vs receta → lista de acciones
  apply.go                  # máquina de estados reanudable (fases, progreso)
  capture.go                # borrador de receta desde un servicio + carpetas
  installed.go              # installed.json
internal/download/          # + varias fuentes y trozos en paralelo
internal/capability/recipe.go   # recipe.apply / status / capture / remove
internal/transport/server.go    # + /files/<receta>/<ruta> con Range
                                #   (http.ServeContent), solo artefactos
internal/catalog/           # nuevo, lado panel: guardar, versionar,
                            # importar/publicar en astrea-services
cmd/astrea-panel/catalog.go     # /api/v1/recipes, instalar, matriz
cmd/astrea-panel/endpoints.go   # /api/v1/endpoints y la pasarela /svc/
cmd/astrea-panel/keys.go        # API keys
cmd/astrea-panel/web/index.html # pestaña Catálogo
cmd/astrea-hub/tools.go         # recipe.* y fleet.endpoints para la IA
recipes/                    # borradores; se mudan a astrea-services
```

`internal/recipe` no importa nada del agente: solo recibe interfaces (descargar,
ejecutar, arrancar servicio). Así se prueba en Linux sin Windows y el panel
puede usar el mismo `Validate` y el mismo `plan` para enseñar qué hará antes de
hacerlo.

## 9. Fases

1. **Receta + instalar en una PC.** Formato, `Validate`, `plan`, `recipe.apply`
   y `recipe.status` (descarga de una fuente, la que ya hay), `/files/` en el
   agente. Prueba: adoptar ComfyUI en f3-1 sin bajar nada, y completar f3-c2
   (le faltan unos 45 GB para igualarla al resto del aula).
2. **Catálogo en el panel.** Guardar recetas, matriz receta × PC, botón
   instalar, progreso. `GET /api/v1/endpoints`.
3. **Copia entre PCs rápida.** Descarga multi-fuente por trozos, campo `lan`,
   depósito. `recipe.capture`.
4. **API keys y pasarela `/svc/`.** Token en `/dyn/` para `auth: key`.
5. **Global + GPU.** `global: true`, aplicación automática a PCs nuevas,
   UI-TARS pasa a receta, `resources` y reparto de VRAM.
6. **Repo `astrea-services`.** Importar y publicar recetas oficiales.

Borradores ya escritos en `recipes/`: `comfyui-minimax` (sale de
`lorian-studio/manifest/models.json` y `bootstrap.ps1`) y `ui-tars` (sale de
`visionprov.go`). `lorian-share` no tiene receta: lo sustituye `/files/`.

Tamaño de los perfiles de `comfyui-minimax`: básico 44 GB, estándar 71 GB,
completo 121 GB, con música 133 GB. Ninguna PC del aula coincide hoy con un
perfil exacto (las de 84 GB tienen parte de la prioridad 3 y no tienen la
LoRA de 8 pasos): al adoptarlas hay que elegir perfil y completar o limpiar.

## 10. Fase 1: qué hay y cómo se probó

Código: `internal/recipe` (formato, `Validate`, `Expand`, `MakePlan`,
`Installer` con `installed.json`, `/files/`) e `internal/capability/recipe.go`
(el `Env` del agente y `recipe.apply` / `plan` / `status` / `cancel` /
`remove`).

Decisiones tomadas al implementar:

- `unpack.creates` es obligatorio: sin él, adoptar una PC volvía a bajar el
  `.7z` de ComfyUI aunque ya estuviera descomprimido.
- Los `.7z` (y todo lo que no es `.zip`) se descomprimen con el `tar` de
  Windows (bsdtar). Probado con un `.7z` real.
- Los configs se comparan ignorando BOM UTF-8 y CRLF: el
  `extra_model_paths.yaml` de las 12 PCs lo escribió PowerShell y, si no,
  se reescribía siempre.
- Al adoptar, un servicio que ya corre con el mismo comando no se reinicia;
  `expose` y `ready` no cuentan como cambio.
- `sources` (otras PCs) va con el token de cada PC y no se guarda nunca. Cada
  fuente tiene 2 intentos si quedan más; la última, 5 (el DNS del aula falla
  a ratos).
- Una instalación cortada por un reinicio del agente se retoma sola al
  arrancar, salvo si la receta usa secretos (no se guardan): entonces queda
  `failed` con el aviso.
- `on_demand` todavía no se acepta en `recipe.apply`: UI-TARS sigue siendo el
  `Provisioner` en Go hasta la fase 5.
- Los servicios no pueden usar `${secret:…}` (ni `root`): el supervisor guarda
  su definición en `services.json` y el secreto quedaría en claro. Si un
  servicio necesita una clave, que la lea de un archivo de `files` (que con
  secretos se escribe con permisos `0600`).
- `auth: key` se acepta, pero hasta la fase 4 no se aplica: el plan avisa de
  que `/dyn/<endpoint>` queda abierto a toda la tailnet.
- `root` tiene que ser absoluto (o empezar por `${data}`): uno relativo caería
  en la carpeta de trabajo del servicio (System32).
- Al descomprimir se deja `.astrea-unpacking-<id>` en `root` hasta terminar
  bien. Si sigue ahí, `unpack.creates` no cuenta y se descomprime otra vez
  (un `tar` muerto a medias pudo haberlo escrito ya).
- Cancelar mata el árbol entero del paso en curso (pip, instaladores), no solo
  el shell, y el sha256 de los modelos se puede interrumpir.
- En `audit.log` y en el feed del panel, `secrets`, `token`, `password` y
  `authorization` salen como `***`. El error de un paso fallido se guarda sin
  los valores de los secretos.
- Un artefacto sin `sha256` (los comprimidos) no reanuda el `.part` de otra
  fuente: nada detectaría dos mitades distintas pegadas.

Pruebas en f3-c2 (agente `v0.6.5-recipesN`, sin publicar):

| Prueba | Resultado |
|---|---|
| Receta pequeña (zip, 7z, hf://, espejo roto, config, 2 pasos, servicio) | Instalada; endpoint `/dyn/` responde |
| Aplicarla otra vez | 0 bytes, pasos "ya hechos", servicio sin reiniciar |
| Copia desde `/files/` de otra receta (fuente mala + buena) | 404 descartado al momento, todo de la buena |
| `/files/` sin token / config / Range | 401 / 404 / 206 |
| Adoptar ComfyUI, perfil `basico` | 0 bytes, ComfyUI con el mismo PID, `/dyn/comfyui` nuevo |
| Completar a `estandar` (23,6 GB de ModelScope, ~8 MB/s, reanudando un `.part` de 3,6 GB) | 52 min; pasos "ya hechos", ComfyUI sin reiniciar |
| `recipe.plan` `estandar` con `verify` | 8 modelos (69 GB) con sha256 correcto en 3 min 50 s; nada que bajar |
| Cancelar a mitad y reaplicar | `.part` conservado, retoma con Range |
| Reiniciar el agente a mitad | Se retoma solo al arrancar |
| `recipe.remove` con `purge` | Servicio, endpoint y archivos fuera |

Tras la revisión de código (`v0.6.5-recipes6`):

| Prueba | Resultado |
|---|---|
| Reaplicar `estandar` con el binario nuevo | 0 bytes, ComfyUI sin reiniciar |
| Receta con `${secret:…}` en un archivo | `audit.log` con `***`; ni `installed.json` ni `services.json` lo llevan |
| Marcador `.astrea-unpacking-7z` a mano y reaplicar | Vuelve a descomprimir solo ese, borra el marcador |
| `root` relativo / secreto en `services.env` | Rechazados por `Validate` |
| Cancelar un paso que lanzó `ping -n 300` | Responde en 0,1 s; el `ping` muere con él |
| `recipe.remove` con `purge` | 8 acciones; `/dyn/astrea-test` da 404 |

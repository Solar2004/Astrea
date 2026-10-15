# Diseño de Astrea

Guía visual y de usabilidad del panel (`cmd/astrea-panel/web/`) y de la marca
(`assets/brand/`). Si tocas la UI, mantén lo que dice aquí; si algo no encaja,
cambia primero este documento.

## Principios

1. **Responder la pregunta del usuario, no volcar datos.** La pregunta real es
   “¿puedo usar este equipo ahora?”. Por eso cada equipo tiene **un solo
   veredicto** (`presence()` en `index.html`): *Libre*, *En uso*, *Sin conexión*
   o *Bloqueada*. Las métricas van debajo.
2. **Lenguaje de persona, no de protocolo.** Nada de `yolo`, `backoff`,
   `session: none` ni `unknown_method` en pantalla. Tabla de traducción abajo.
   El detalle técnico se muestra solo en pequeño, debajo de la explicación.
3. **Cada error dice qué hacer.** “No se puede contactar” va con una lista de
   comprobaciones; “versión antigua” dice cómo arreglarlo.
4. **Nunca color solo.** Todo estado lleva texto (chip con punto + etiqueta).
   Ámbar y rojo no se distinguen bien con daltonismo deutan (ΔE 3,0, medido con
   el validador de la skill dataviz), así que la etiqueta es obligatoria.
5. **Sin diálogos del navegador.** Las acciones peligrosas se confirman en el
   propio botón (`confirmable()`: “Actualizar” → “Cancelar / Sí, actualizar”).
6. **Cero build.** HTML/CSS/JS vanilla embebido en el binario. Tomamos la
   *estética* de HeroUI (chips “flat”, radios generosos, sombras suaves) y de
   Apple (barra lateral translúcida, títulos grandes, control segmentado, listas
   agrupadas tipo Ajustes) sin traer React, Tailwind ni Node.

## Marca

| Archivo (`assets/brand/`) | Uso |
|---|---|
| `astrea-icon.svg` | Icono maestro 1024 (squircle, estrella, órbita, nodo). |
| `astrea-favicon.svg` | Versión para ≤ 64 px: sin órbita, estrella más gruesa. |
| `astrea-glyph.svg` | Solo la estrella + nodo en `currentColor` (monocromo, sellos, CLI). |
| `astrea-logo.svg` / `astrea-logo-dark.svg` | Logotipo horizontal para fondo claro / oscuro (texto convertido a trazos, Sora 600). |
| `astrea-icon-{16…1024}.png` | Rasterizados. 512 → foto del bot de Telegram; 180 → apple-touch-icon. |
| `astrea.ico` | 16/32/64/256 para el MSI (`packaging/wix`) y el `.exe` de Windows. |

**Concepto.** Astrea es la doncella de las estrellas. Una estrella de cuatro
puntas (la guía) con una órbita que llega a un **nodo verde menta**: la máquina
remota a la que la IA llega por la red privada.

**Colores de marca:** noche `#1B1F4B` → índigo `#2A2470` → violeta `#4B2A8C`;
estrella `#FFFFFF`→`#CFE0FF`; nodo `#7CF0C8`.

Regenerar los PNG/ICO desde el SVG:

```sh
cd assets/brand
for s in 180 256 512 1024; do rsvg-convert -w $s -h $s astrea-icon.svg -o astrea-icon-$s.png; done
for s in 16 32 64; do rsvg-convert -w $s -h $s astrea-favicon.svg -o astrea-icon-$s.png; done
magick astrea-icon-16.png astrea-icon-32.png astrea-icon-64.png astrea-icon-256.png astrea.ico
```

## Tokens de UI

Definidos en `:root` de `index.html`, con modo claro y oscuro (sigue al sistema).

| Token | Claro | Oscuro | Uso |
|---|---|---|---|
| `--primary` | `#5B4BDB` | `#8B7CFF` | Acciones, selección, serie única de las gráficas |
| `--good` | `#12A150` | `#45D483` | Libre, funcionando |
| `--warn` | `#B7791F` | `#F5A524` | En uso, arrancando, reintentando |
| `--bad` | `#E5484D` | `#FF6369` | Sin conexión, bloqueada, disco/RAM ≥ 90 % |
| `--bg` / `--surface` | `#F5F5F7` / `#FFF` | `#0E0E12` / `#18181D` | Fondo / tarjetas |

Radios: tarjetas 16, botones 10, chips redondos. Tipografía del sistema
(SF / Segoe / Inter), números tabulares. Iconos: set propio de 24 px y trazo 1,8
(estilo SF Symbols) en el `<svg>` de símbolos al inicio del `<body>`.

## Arquitectura de información

```
Barra lateral                     Contenido
───────────────                   ─────────────────────────────────────────
✦ Astrea
▦ Resumen          ──────────▶    KPIs (en línea · libres · en uso · servicios)
EQUIPOS                           avisos (actualizar / reinstalar)
● pc-aula   Libre   3%            tarjetas de equipos → detalle
● pc-aula2  Sin conexión
AYUDA                             Detalle de equipo  (#/m/NOMBRE)
⊕ Añadir un equipo                  [Frenar IA] [Ver pantalla] [Actualizar]
───────────────                     Rendimiento | Servicios | Actividad | Sistema
● Actualizado 19:31
  Astrea v0.3.0                   Ayuda  (#/ayuda): 3 pasos + glosario de estados
```

En móvil (< 860 px) la barra lateral pasa a un cajón con botón ☰. Es una PWA
(`manifest.webmanifest`), así que se puede instalar como app en el escritorio o
en el móvil.

## Traducción de estados

| Backend | En pantalla |
|---|---|
| `online:false` | **Sin conexión** · “Vista por última vez hace…” |
| `admin.locked` | **Bloqueada** · freno de emergencia |
| `idle.session == "none"` | **Libre** · “Nadie ha iniciado sesión” |
| `idle.user_active` | **En uso** · “Alguien la está usando · proceso” |
| `admin.mode == "readonly"` / otro | **Solo lectura** / **Control total** |
| service `running/starting/backoff/stopped/failed` | Funcionando / Arrancando / Reintentando / Detenido / Falló |
| sin `admin.version` | **Actualización** (agente < v0.3; `POST /api/update` lo migra en remoto) |

## Endpoints que usa la UI

| Endpoint | Dónde en la UI |
|---|---|
| `GET /api/stats` (incl. `admin.*`, `last_seen`) | Todo; “Vista por última vez” |
| `GET /api/screenshot?machine=` | Botón *Ver pantalla* (interruptor *En vivo*: cada 3 s) |
| `GET /api/version`, `POST /api/update?machine=NOMBRE\|all\|hub` | Avisos del Resumen, botón *Actualizar agente* |
| `POST /api/lock?machine=&locked=` | *Frenar IA* (cabecera), aviso rojo y *Sistema › Freno de emergencia* |
| `POST /api/machines`, `DELETE /api/machines?name=` | *Añadir un equipo* (paso 3: pegar ficha) y *Sistema › Quitar del panel* |
| `GET /api/audit?machine=&limit=` | Pestaña *Actividad*; 501 → estado vacío “actualízalo” |
| `POST /api/service?…&action=`, `GET /api/service/logs` | Pestaña *Servicios*: Iniciar / Reiniciar / Detener / Registro |

Cada pestaña tiene URL propia: `#/m/NOMBRE/rendimiento|servicios|actividad|sistema`.
La pestaña *Actividad* oculta por defecto las consultas que hace el propio panel
(`sys.stats`, `admin.status`…), si no taparían lo que hace la IA.

### Pendiente

- **Pantalla en vivo de verdad**: `GET /api/screen/stream?machine=` (MJPEG). Hoy es polling.

## Bot de Telegram

El bot es el panel de bolsillo: misma marca, mismo vocabulario, menos texto.
Regla de oro: **una imagen dice el estado, el pie de foto dice qué hacer, los
botones lo hacen.**

### Tarjetas (imágenes)

`cmd/astrea-panel/tgcard.go` dibuja PNG de 1080 px de ancho con el tema oscuro
del panel (Go puro, fuentes Manrope/Sora embebidas en `cmd/astrea-panel/fonts/`):

| Función | Qué muestra |
|---|---|
| `renderFleetCard(list)` | “Tu flota”: 4 KPIs (en línea, libres, en uso, servicios) + una fila por equipo con veredicto y barras CPU/RAM/GPU |
| `renderMachineCard(s, p.history(name))` | Un equipo: veredicto, SO, encendido; CPU, Memoria y Gráfica con número grande + gráfica de área de 2 h (máx. marcado); discos y servicios |

Huecos en la gráfica = equipo apagado (valores < 0). Con 0–1 muestras sale
“Recogiendo datos…”. La GPU que se enseña la elige `pickGPU()`: la que informa
de carga y, si no, la de más memoria (la NVIDIA antes que la Intel integrada).
**El historial debe usar la misma GPU** (`pickGPU`), si no, número y gráfica no cuadran.

### Comandos (registrados con `setMyCommands`, salen en el menú “/”)

| Comando | Respuesta |
|---|---|
| `/flota` (y `/estado`, botón 📊) | Tarjeta de flota + botones de cada equipo |
| `/equipo NOMBRE` (alias `/stats`) | Tarjeta del equipo + botones del equipo |
| `/pantalla NOMBRE` | Captura + `🔄 Otra` · `← Equipo` |
| `/actividad NOMBRE` | Últimas 10 acciones de la IA, con los verbos del panel (“Ejecutó un comando…”), sin las consultas automáticas |
| `/frenar NOMBRE` · `/liberar NOMBRE` | Freno de emergencia, **con confirmación** |
| `/avisos` | Activar/desactivar avisos |

Si falta `NOMBRE` y solo hay un equipo, usarlo; si hay varios, responder con los
botones de equipos en vez de con un error.

### Botones (inline)

```
Tarjeta de flota                         Tarjeta de equipo
┌──────────────┬──────────────┐          ┌──────────────┬──────────────┐
│ 🟢 f3-1      │ 🟡 pc-aula2  │          │ 🖥 Pantalla  │ 🔄 Actualizar│
├──────────────┼──────────────┤          ├──────────────┼──────────────┤
│ 🔴 pc-aula   │              │          │ ⚙️ Servicios │ 📜 Actividad │
├──────────────┴──────────────┤          ├──────────────┴──────────────┤
│ 🔄 Actualizar               │          │ 🔒 Frenar IA                 │
└─────────────────────────────┘          ├─────────────────────────────┤
                                          │ ← Flota                      │
                                          └─────────────────────────────┘
```

- **🔄 Actualizar edita el mismo mensaje** (`editMessageMedia`) en vez de mandar
  otro: el chat no se llena de tarjetas viejas. Igual al pasar de flota a equipo
  y al volver con `← Flota`: es una sola “pantalla” que cambia.
- Mientras se genera: `answerCallbackQuery` con “Cargando…” (no dejar el reloj girando).
- 🔒 Frenar IA → el mensaje cambia a “¿Frenar la IA en f3-1? No podrá hacer nada
  hasta que lo liberes.” con `Sí, frenar` / `Cancelar`. Con el freno puesto el
  botón pasa a `🔓 Liberar` (misma confirmación).
- ⚙️ Servicios → lista en texto con un botón por servicio: `↻ llama` (reiniciar),
  y `⏹`/`▶` según estado. Detener pide confirmación.
- Teclado fijo de abajo: `📊 Flota` · `🟢 Activas` (el de ahora, renombrado).

### Pies de foto

Cortos, en HTML, mismo vocabulario y emojis siempre:
🟢 Libre · 🟡 En uso · 🔴 Sin conexión · 🔒 Bloqueada.

- Flota: `🛰 <b>2 de 3 en línea</b> · 1 libre · 1 en uso`
- Equipo: `🟢 <b>f3-1</b> · Libre · sin actividad desde hace 1 h`
- Equipo con problema: añadir una línea de qué hacer
  (`🔴 No responde: ¿está encendido y con Tailscale?`).

### Avisos (proactivos)

- Encendido/apagado: los de ahora, con botón `Ver equipo`.
- **Umbrales** (una vez, con 30 min de enfriamiento por equipo y métrica):
  GPU ≥ 85 °C, disco ≥ 90 %, RAM ≥ 95 % sostenida 2 min, servicio que pasa a
  `failed`. Texto: `🌡 <b>f3-1</b>: la gráfica está a 88 °C` + `Ver equipo`.
- Freno activado/desactivado (desde el panel o la IA): siempre avisar.
- `/avisos` permite silenciarlos; los de freno no se silencian.

### Foto de perfil

`assets/brand/astrea-icon-512.png`, puesta a mano en @BotFather (la Bot API no
deja cambiarla).

### Fuentes de las tarjetas

Si un texto nuevo usa caracteres fuera del subset, regenerarlo:

```sh
G=~/.local/share/fonts/google   # Manrope[wght].ttf y Sora[wght].ttf (OFL)
fonttools varLib.instancer "$G/Manrope[wght].ttf" wght=500 -o /tmp/m500.ttf
pyftsubset /tmp/m500.ttf --text="…caracteres…" --output-file=cmd/astrea-panel/fonts/manrope-500.ttf --no-hinting
```

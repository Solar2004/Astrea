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

### Para el bot de Telegram

- Foto de perfil: `assets/brand/astrea-icon-512.png` (vía @BotFather).
- Mismo vocabulario que el panel: Libre / En uso / Sin conexión / Bloqueada,
  con los mismos emojis siempre: 🟢 Libre · 🟡 En uso · 🔴 Sin conexión · 🔒 Bloqueada.

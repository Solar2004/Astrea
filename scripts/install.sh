#!/usr/bin/env bash
# Instalador de Astrea para Linux.
#
#   install.sh hub                         Instala el control (hub + panel) en
#                                          tu PC principal, junto a Hermes.
#   install.sh agent                       Instala el agente en una PC Linux a
#                                          controlar (corre como servicio).
#   install.sh add-machine NAME IP TOKEN   Registra una PC en ~/.config/astrea
#                                          (la usan el hub y el panel).
#
# Variables opcionales:
#   ASTREA_REPO        owner/repo (default Solar2004/Astrea)
#   ASTREA_PORT        puerto del agente (default 7700)
#   ASTREA_NO_TAILSCALE=1   no tocar Tailscale (ya lo gestionas tú)
#   ASTREA_TS_AUTHKEY  auth key para conectar Tailscale sin navegador
set -euo pipefail

REPO="${ASTREA_REPO:-Solar2004/Astrea}"
# Cómo volver a invocar este script en los mensajes: con `curl | bash`, $0 es
# "bash", así que se muestra la línea completa.
if [ -f "$0" ] && [ "$(basename "$0")" = "install.sh" ]; then
  SELF="$0"
else
  SELF="curl -fsSL https://raw.githubusercontent.com/${REPO}/main/scripts/install.sh | bash -s --"
fi
PORT="${ASTREA_PORT:-7700}"
ROLE="${1:-hub}"
BINDIR="${HOME}/.local/bin"
CFGDIR="${XDG_CONFIG_HOME:-$HOME/.config}/astrea"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) GOARCH=amd64 ;;
  aarch64 | arm64) GOARCH=arm64 ;;
  *)
    echo "arquitectura no soportada: $ARCH"
    exit 1
    ;;
esac

step() { printf '\n== %s\n' "$*"; }
ok() { printf '   OK  %s\n' "$*"; }
warn() { printf '   !!  %s\n' "$*"; }
die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

# sudo wrapper: usa sudo solo si no somos root.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi

mkdir -p "$BINDIR" "$CFGDIR"

download() {
  local name="$1"
  local url="https://github.com/${REPO}/releases/latest/download/${name}-linux-${GOARCH}"
  echo "Descargando $name ..."
  curl -fsSL "$url" -o "${BINDIR}/${name}.part" || die "no se pudo descargar $url"
  chmod +x "${BINDIR}/${name}.part"
  mv -f "${BINDIR}/${name}.part" "${BINDIR}/${name}"
  ok "${name} -> ${BINDIR}/${name}"
}

# ensure_tailscale instala Tailscale si falta, arranca el demonio y conecta el
# equipo al tailnet (login por navegador, o ASTREA_TS_AUTHKEY sin navegador).
ensure_tailscale() {
  if [ "${ASTREA_NO_TAILSCALE:-0}" = "1" ]; then
    return 0
  fi
  step "Tailscale (red privada entre tus equipos)"
  if ! command -v tailscale >/dev/null 2>&1; then
    echo "   Instalando Tailscale ..."
    if command -v pacman >/dev/null 2>&1; then
      $SUDO pacman -S --needed --noconfirm tailscale || die "no se pudo instalar tailscale con pacman"
    else
      curl -fsSL https://tailscale.com/install.sh | $SUDO sh || die "no se pudo instalar Tailscale"
    fi
  fi
  $SUDO systemctl enable --now tailscaled 2>/dev/null || true
  # ¿Ya conectado?
  if tailscale ip -4 >/dev/null 2>&1; then
    ok "Tailscale conectado: $(tailscale ip -4 | head -n1)"
    return 0
  fi
  step "Conectando este equipo a tu tailnet"
  echo "   Inicia sesión con la MISMA cuenta de Tailscale que tus otras PCs."
  if [ -n "${ASTREA_TS_AUTHKEY:-}" ]; then
    $SUDO tailscale up --authkey "${ASTREA_TS_AUTHKEY}" || die "tailscale up falló"
  else
    echo "   Si no se abre el navegador, copia el enlace que aparezca abajo."
    $SUDO tailscale up || die "tailscale up falló (acepta el login en el navegador y reintenta)"
  fi
  ok "Tailscale conectado: $(tailscale ip -4 | head -n1)"
}

# install_panel_service deja el panel corriendo en localhost y arrancando solo.
install_panel_service() {
  local unit="${HOME}/.config/systemd/user/astrea-panel.service"
  mkdir -p "$(dirname "$unit")"
  cat >"$unit" <<UNIT
[Unit]
Description=Astrea panel — dashboard de hardware de la flota (localhost:7780)
After=network-online.target

[Service]
ExecStart=${BINDIR}/astrea-panel --config ${CFGDIR}/machines.json --listen 127.0.0.1:7780 --open=false
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload 2>/dev/null || true
  if systemctl --user enable --now astrea-panel.service 2>/dev/null; then
    loginctl enable-linger "$USER" 2>/dev/null || true
    ok "Panel en http://127.0.0.1:7780 (arranca solo al iniciar sesión)"
  else
    warn "No pude registrar el servicio de usuario del panel; lánzalo a mano con 'astrea-panel'."
  fi
}

# install_update_timer: una vez al día `astrea-hub update` pone hub, panel y
# los agentes de la flota en la última release (los agentes además se
# actualizan solos). Desactivar: systemctl --user disable --now astrea-update.timer
install_update_timer() {
  local dir="${HOME}/.config/systemd/user"
  mkdir -p "$dir"
  cat >"${dir}/astrea-update.service" <<UNIT
[Unit]
Description=Astrea — actualizar hub, panel y agentes a la última release
After=network-online.target

[Service]
Type=oneshot
ExecStart=${BINDIR}/astrea-hub update --config ${CFGDIR}/machines.json
UNIT
  cat >"${dir}/astrea-update.timer" <<UNIT
[Unit]
Description=Astrea — buscar actualizaciones cada día

[Timer]
OnBootSec=15min
OnUnitActiveSec=1d
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl --user daemon-reload 2>/dev/null || true
  if systemctl --user enable --now astrea-update.timer 2>/dev/null; then
    ok "Actualizaciones automáticas: cada día (astrea-update.timer)"
  else
    warn "No pude activar astrea-update.timer; actualiza a mano con 'astrea-hub update'."
  fi
}

# seed_machines crea un machines.json de ejemplo si no hay ninguno, para que el
# hub y el panel tengan dónde leer.
seed_machines() {
  if [ -f "${CFGDIR}/machines.json" ] || [ -f "${CFGDIR}/panel.json" ]; then
    return 0
  fi
  cat >"${CFGDIR}/machines.json" <<'JSON'
{
  "machines": []
}
JSON
  ok "Config creada en ${CFGDIR}/machines.json (aún sin PCs; usa 'install.sh add-machine')"
}

case "$ROLE" in
hub)
  ensure_tailscale
  step "Instalando el control"
  download astrea-hub
  download astrea-panel
  seed_machines
  install_panel_service
  install_update_timer
  cat <<EOF

==================== LISTO (control) ====================
Hub y panel instalados en ${BINDIR}.

1) Instala el agente en cada PC a controlar (Windows o Linux). Cada instalador
   te da una ficha (hub-config.txt) con su IP de tailnet y su token.
   Lo más fácil: ábrela, cópiala entera y pégala en el panel
   (http://127.0.0.1:7780 → Añadir un equipo). O por terminal:

     ${SELF} add-machine <nombre> <ip-tailnet> <token>

2) Engancha el hub a Hermes (una vez). En ~/.hermes/config.yaml — o en el
   perfil que uses — bajo mcp_servers:

     astrea:
       command: ${BINDIR}/astrea-hub
       args: ["--config","${CFGDIR}/machines.json"]
       enabled: true

   Reinicia Hermes: aparecerán las herramientas astrea.*

3) Panel de hardware: http://127.0.0.1:7780

4) Actualizaciones: automáticas (agentes cada ~6 h, hub y panel cada día).
   A mano: astrea-hub update   ·   solo mirar: astrea-hub update --check
=========================================================
EOF
  ;;

agent)
  ensure_tailscale
  step "Instalando el agente"
  download astrea-agent
  [ "${ASTREA_NO_TAILSCALE:-0}" = "1" ] && command -v tailscale >/dev/null 2>&1 || true
  IP="$(tailscale ip -4 2>/dev/null | head -n1 || true)"
  [ -n "$IP" ] || die "este equipo no tiene IP de tailnet; conecta Tailscale y reintenta"
  TOKEN="${ASTREA_TOKEN:-$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
  cat >"${CFGDIR}/config.json" <<JSON
{
  "addr": "${IP}:${PORT}",
  "token": "${TOKEN}",
  "data_dir": "${CFGDIR}",
  "mode": "yolo",
  "keep_awake": "auto"
}
JSON
  chmod 600 "${CFGDIR}/config.json"
  UNITDIR="${HOME}/.config/systemd/user"
  mkdir -p "$UNITDIR"
  cat >"${UNITDIR}/astrea-agent.service" <<UNIT
[Unit]
Description=Astrea agent
After=network-online.target

[Service]
ExecStart=${BINDIR}/astrea-agent --config ${CFGDIR}/config.json
# El agente se actualiza solo: sale con código 75 y systemd arranca el nuevo.
Environment=ASTREA_SUPERVISED=1
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable --now astrea-agent.service
  loginctl enable-linger "$USER" 2>/dev/null || true
  command -v xdotool >/dev/null 2>&1 || warn "instala xdotool para mouse/teclado/captura en X11"
  cat <<EOF

==================== LISTO (agente) ====================
Agente corriendo. IP tailnet: ${IP}   Puerto: ${PORT}
En tu PC principal (la del hub), registra esta PC con:

   install.sh add-machine $(hostname) ${IP} ${TOKEN}
========================================================
EOF
  ;;

add-machine)
  NAME="${2:-}"
  MIP="${3:-}"
  MTOKEN="${4:-}"
  [ -n "$NAME" ] && [ -n "$MIP" ] && [ -n "$MTOKEN" ] || die "uso: ${SELF} add-machine <nombre> <ip-tailnet> <token>"
  # Un solo archivo sirve al hub y al panel: el hub lee machines.json y cae a
  # panel.json; aquí usamos machines.json.
  FILE="${CFGDIR}/machines.json"
  [ -f "$FILE" ] || echo '{"machines": []}' >"$FILE"
  WS="ws://${MIP}:${PORT}/ws"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$FILE" "$NAME" "$WS" "$MTOKEN" <<'PY'
import json, sys
path, name, ws, token = sys.argv[1:5]
try:
    d = json.load(open(path))
except Exception:
    d = {}
ms = d.get("machines") or []
ms = [m for m in ms if m.get("name") != name]  # reemplaza si ya existía
ms.append({"name": name, "ws": ws, "token": token})
d["machines"] = ms
json.dump(d, open(path, "w"), indent=2, ensure_ascii=False)
print(f"   OK  {name} -> {ws} ({len(ms)} máquina(s) en total)")
PY
  else
    die "necesito python3 para editar ${FILE} de forma segura; añádela a mano"
  fi
  echo "   El panel la toma sola; reinicia Hermes para ver sus herramientas."
  ;;

*)
  echo "uso: install.sh [hub | agent | add-machine NAME IP TOKEN]"
  exit 1
  ;;
esac

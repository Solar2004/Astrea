#!/usr/bin/env bash
# Instalador de Astrea para Linux.
#   install.sh hub     -> instala astrea-hub (en tu PC principal, junto a Hermes)
#   install.sh agent   -> instala astrea-agent (en una PC Linux a controlar)
# Variables opcionales: ASTREA_REPO (default Solar2004/Astrea), ASTREA_PORT (7700).
set -euo pipefail

REPO="${ASTREA_REPO:-Solar2004/Astrea}"
PORT="${ASTREA_PORT:-7700}"
ROLE="${1:-hub}"
BINDIR="${HOME}/.local/bin"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "arquitectura no soportada: $ARCH"; exit 1 ;;
esac

mkdir -p "$BINDIR"

download() {
  local name="$1"
  local url="https://github.com/${REPO}/releases/latest/download/${name}-linux-${GOARCH}"
  echo "Descargando $name desde $url ..."
  curl -fsSL "$url" -o "${BINDIR}/${name}"
  chmod +x "${BINDIR}/${name}"
}

case "$ROLE" in
  hub)
    download astrea-hub
    echo
    echo "astrea-hub instalado en ${BINDIR}/astrea-hub"
    echo "Añade a ~/.hermes/config.yaml bajo mcp_servers (reemplaza IP y token):"
    cat <<YAML
  astrea:
    command: ${BINDIR}/astrea-hub
    args: ["--ws","ws://100.x.y.z:${PORT}/ws","--caps","http://100.x.y.z:${PORT}/capabilities","--token","<TOKEN>"]
    enabled: true
YAML
    ;;
  agent)
    download astrea-agent
    if ! command -v tailscale >/dev/null 2>&1; then
      echo "Instala Tailscale (curl -fsSL https://tailscale.com/install.sh | sh) y 'sudo tailscale up', luego reintenta." ; exit 1
    fi
    IP="$(tailscale ip -4 | head -n1)"
    TOKEN="${ASTREA_TOKEN:-$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
    CFGDIR="${XDG_CONFIG_HOME:-$HOME/.config}/astrea"
    mkdir -p "$CFGDIR"
    cat > "${CFGDIR}/config.json" <<JSON
{
  "addr": "${IP}:${PORT}",
  "token": "${TOKEN}",
  "data_dir": "${CFGDIR}"
}
JSON
    # systemd --user service.
    UNITDIR="${HOME}/.config/systemd/user"
    mkdir -p "$UNITDIR"
    cat > "${UNITDIR}/astrea-agent.service" <<UNIT
[Unit]
Description=Astrea agent
After=network-online.target

[Service]
ExecStart=${BINDIR}/astrea-agent --config ${CFGDIR}/config.json
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNIT
    systemctl --user daemon-reload
    systemctl --user enable --now astrea-agent.service
    echo
    echo "astrea-agent corriendo. IP tailnet: ${IP}  Token: ${TOKEN}"
    echo "(para mouse/captura en X11 instala xdotool)"
    ;;
  *)
    echo "uso: install.sh [hub|agent]"; exit 1 ;;
esac

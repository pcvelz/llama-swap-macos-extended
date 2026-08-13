#!/usr/bin/env bash
# Installer for the llama-swap-macos-extended fork (Linux).
# Downloads the llama-swap binary and the system-tray helper into ~/.local/bin
# and writes a starter config to ~/.config/llama-swap/config.yaml if none
# exists. The tray helper is launched automatically by llama-swap (menu_bar is
# on by default); it needs a StatusNotifierItem/AppIndicator-capable desktop
# (KDE and most desktops work out of the box; GNOME needs the AppIndicator
# extension).
set -euo pipefail

REPO="pcvelz/llama-swap-macos-extended"
BASE="https://github.com/${REPO}/releases/latest/download"
DEST="${HOME}/.local/bin"
CONFIG_DIR="${HOME}/.config/llama-swap"

if [ "$(uname -s)" != "Linux" ]; then
  echo "This installer is for Linux only (see install-macos.sh / install-windows.ps1)." >&2
  exit 1
fi

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

mkdir -p "$DEST"

for pair in "llama-swap-linux-${ARCH}:llama-swap" "llama-swap-tray-linux-${ARCH}:llama-swap-tray"; do
  asset="${pair%%:*}"
  target="${pair##*:}"
  echo "Downloading ${asset}..."
  curl -fsSL "${BASE}/${asset}" -o "${DEST}/${target}"
  chmod +x "${DEST}/${target}"
done

if [ ! -f "${CONFIG_DIR}/config.yaml" ]; then
  mkdir -p "$CONFIG_DIR"
  cat > "${CONFIG_DIR}/config.yaml" <<'YAML'
# llama-swap starter config - add your models below.
# Full reference: https://github.com/pcvelz/llama-swap-macos-extended

models:
  # example:
  #   cmd: llama-server --port ${PORT} --model /path/to/model.gguf

# menu_bar: system-tray helper (on by default)
# bars: 1-2 of gpu, vram, cpu, ram
menu_bar:
  enabled: true
  bars: [gpu, vram]
YAML
  echo "Wrote starter config to ${CONFIG_DIR}/config.yaml"
fi

echo
echo "Installed llama-swap and llama-swap-tray to ${DEST}."
case ":${PATH}:" in
  *":${DEST}:"*) ;;
  *) echo "Note: add ${DEST} to your PATH to run them by name." ;;
esac
echo "Start with:  llama-swap --config ${CONFIG_DIR}/config.yaml --listen localhost:8080"

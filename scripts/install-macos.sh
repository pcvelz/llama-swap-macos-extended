#!/usr/bin/env bash
# Installer for the llama-swap-macos-extended fork (macOS).
# Downloads the llama-swap binary and the macOS menu-bar helper, clears the
# macOS quarantine flag, and installs both into ~/bin. The menu-bar helper is
# on by default, so once llama-swap is started the menu icon appears on its own.
set -euo pipefail

REPO="pcvelz/llama-swap-macos-extended"
BASE="https://github.com/${REPO}/releases/latest/download"
DEST="${HOME}/bin"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "This installer is for macOS only." >&2
  exit 1
fi

mkdir -p "$DEST"

for pair in "llama-swap-darwin-arm64:llama-swap" "llama-swap-menu:llama-swap-menu"; do
  asset="${pair%%:*}"
  target="${pair##*:}"
  echo "Downloading ${asset}..."
  curl -fsSL "${BASE}/${asset}" -o "${DEST}/${target}"
  chmod +x "${DEST}/${target}"
  xattr -d com.apple.quarantine "${DEST}/${target}" 2>/dev/null || true
done

echo "Installed llama-swap and llama-swap-menu to ${DEST}."
case ":${PATH}:" in
  *":${DEST}:"*) ;;
  *) echo "Note: add ${DEST} to your PATH to run them by name." ;;
esac

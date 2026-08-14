#!/bin/bash
# Local build + install for ~/bin/llama-swap - ALWAYS use this instead of a
# bare `go build -o ~/bin/llama-swap .`.
#
# WHY the codesign step is load-bearing: macOS TCC keys the "access files in
# Documents" grant to the binary's code signature. A bare go build ships a
# fresh ad-hoc signature every time, so every rebuild re-prompts the user.
# Signing with the stable identity + identifier below keeps one persistent
# grant across rebuilds (first prompt after adopting this script is the last).
#
# WHY the `embed_ui` tag is load-bearing: upstream gates the embedded web UI
# behind that build tag (internal/server/embed.go). A bare `go build` produces
# a binary that serves no /ui at all, so the tag - and the npm build that fills
# internal/server/ui_dist - are both required. Set SKIP_UI=1 to reuse the
# existing ui_dist and skip the npm step.
set -euo pipefail
cd "$(dirname "$0")"
IDENTITY="Apple Development: Peter van Velzen (VB3WV9PM73)"
DEST="${1:-$HOME/bin/llama-swap}"
if [ "${SKIP_UI:-0}" != "1" ]; then
  make ui
elif [ ! -d internal/server/ui_dist ]; then
  echo "SKIP_UI=1 but internal/server/ui_dist is missing - run without SKIP_UI once" >&2
  exit 1
fi
[ -f "$DEST" ] && cp "$DEST" "$DEST.prev"
go build -tags embed_ui -o "$DEST" .
codesign --force --sign "$IDENTITY" --identifier com.llama-swap.local "$DEST"
codesign -dv "$DEST" 2>&1 | grep -E "Identifier|TeamIdentifier"
echo "installed: $DEST (previous kept at $DEST.prev)"

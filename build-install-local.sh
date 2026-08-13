#!/bin/bash
# Local build + install for ~/bin/llama-swap - ALWAYS use this instead of a
# bare `go build -o ~/bin/llama-swap .`.
#
# WHY the codesign step is load-bearing: macOS TCC keys the "access files in
# Documents" grant to the binary's code signature. A bare go build ships a
# fresh ad-hoc signature every time, so every rebuild re-prompts the user.
# Signing with the stable identity + identifier below keeps one persistent
# grant across rebuilds (first prompt after adopting this script is the last).
set -euo pipefail
cd "$(dirname "$0")"
IDENTITY="Apple Development: Peter van Velzen (VB3WV9PM73)"
DEST="${1:-$HOME/bin/llama-swap}"
[ -f "$DEST" ] && cp "$DEST" "$DEST.prev"
go build -o "$DEST" .
codesign --force --sign "$IDENTITY" --identifier com.llama-swap.local "$DEST"
codesign -dv "$DEST" 2>&1 | grep -E "Identifier|TeamIdentifier"
echo "installed: $DEST (previous kept at $DEST.prev)"

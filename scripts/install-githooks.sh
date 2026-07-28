#!/usr/bin/env bash
#
# install-githooks.sh — point this clone's git hooks at the tracked .githooks/ dir.
#
# Run once per clone:
#   bash scripts/install-githooks.sh
#
# Uses core.hooksPath rather than copying into .git/hooks so the hooks stay
# version-controlled: an update to .githooks/pre-commit reaches every clone on
# the next pull instead of silently drifting per machine.
#
# Undo with:  git config --unset core.hooksPath
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "not a git repository: $REPO" >&2
    exit 1
fi

chmod +x .githooks/* 2>/dev/null || true
chmod +x scripts/preflight.sh 2>/dev/null || true

CURRENT="$(git config core.hooksPath || true)"
if [[ "$CURRENT" == ".githooks" ]]; then
    echo "core.hooksPath already set to .githooks — nothing to do."
else
    if [[ -n "$CURRENT" ]]; then
        echo "note: core.hooksPath was '$CURRENT'; overwriting with .githooks"
    fi
    git config core.hooksPath .githooks
    echo "core.hooksPath -> .githooks"
fi

echo
echo "installed hooks:"
for h in .githooks/*; do
    [[ -f "$h" ]] || continue
    printf '  %s%s\n' "$(basename "$h")" "$([[ -x "$h" ]] && echo '' || echo '  (NOT EXECUTABLE)')"
done

echo
echo "pre-commit runs scripts/preflight.sh, which mirrors .github/workflows/."
echo "Bypass for one commit:  LLAMA_SWAP_PREFLIGHT=quick|skip git commit ..."

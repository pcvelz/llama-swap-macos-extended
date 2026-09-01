#!/bin/bash
# Install llama-swap - download a matching upstream release binary, or build
# from source when the requested commit has no matching release. That is the
# normal case on a fork (which has no release tags of its own) and also
# covers building an unreleased upstream commit correctly instead of the
# previous silent "fall back to latest" behaviour.
# Usage: ./install-llama-swap.sh [version]
#   version: full commit hash, release version number (e.g. "170"), or "latest" (default)
#
# LLAMA_SWAP_REPO (env) - git URL to build from when a source build is needed.
# Defaults to upstream; build-image.sh sets it to the running repository.
set -e

VERSION="${1:-latest}"
UPSTREAM_REPO="mostlygeek/llama-swap"
LLAMA_SWAP_REPO="${LLAMA_SWAP_REPO:-https://github.com/${UPSTREAM_REPO}.git}"
SRC=/src/llama-swap

mkdir -p /install/bin

# Build the llama-swap binary (with embedded Svelte UI) from source at $1,
# mirroring the Makefile's `ui` + `linux-*` targets: build the UI first, then
# `go build -tags embed_ui` with the same version ldflags.
build_from_source() {
    local ref="$1"

    echo "=== Cloning ${LLAMA_SWAP_REPO} ==="
    git clone --filter=blob:none --no-checkout "${LLAMA_SWAP_REPO}" "${SRC}"
    git -C "${SRC}" checkout --detach "${ref}"

    echo "=== Building UI (ui-svelte) ==="
    ( cd "${SRC}/ui-svelte" && npm install && npm run build )

    local git_hash git_version build_date
    git_hash="$(git -C "${SRC}" rev-parse --short HEAD)"
    git_version="$(git -C "${SRC}" describe --abbrev=6 --tags 2>/dev/null || echo devel)"
    build_date="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

    echo "=== Building llama-swap from source (${git_version}, ${git_hash}) ==="
    (
        cd "${SRC}"
        CGO_ENABLED=0 go build -tags embed_ui \
            -ldflags="-X main.commit=${git_hash} -X main.version=${git_version} -X main.date=${build_date}" \
            -o /install/bin/llama-swap .
    )

    echo "${git_version}" > /install/llama-swap-version
}

# If a full commit hash is given, check whether it matches an upstream
# release tag. A fork's own commits never do, which is what routes them to a
# source build instead of downloading (or misleadingly falling back to) an
# unrelated upstream binary.
if echo "${VERSION}" | grep -qE '^[0-9a-f]{40}$'; then
    echo "=== Resolving commit ${VERSION:0:7} against upstream releases ==="
    TAG=$(git ls-remote --tags "https://github.com/${UPSTREAM_REPO}.git" 2>/dev/null \
        | grep "^${VERSION}" | sed 's|.*refs/tags/||' | grep -v '\^{}' | head -1)
    if [ -n "${TAG}" ]; then
        echo "Resolved to upstream tag: ${TAG}"
        VERSION="${TAG#v}"
    else
        echo "No upstream release matches commit ${VERSION:0:7}; building from source"
        build_from_source "${VERSION}"
        echo "=== llama-swap ($(cat /install/llama-swap-version)) installed ==="
        ls -la /install/bin/llama-swap
        exit 0
    fi
fi

# Strip leading 'v' prefix so both "198" and "v198" work
VERSION="${VERSION#v}"

# Resolve "latest" to actual version number
if [ "$VERSION" = "latest" ]; then
    echo "=== Resolving latest llama-swap release ==="
    VERSION=$(curl -fsSL "https://api.github.com/repos/${UPSTREAM_REPO}/releases/latest" \
        | grep '"tag_name"' | head -1 | cut -d'"' -f4 | sed 's/^v//')
    if [ -z "$VERSION" ]; then
        echo "FATAL: Could not determine latest release version" >&2
        exit 1
    fi
    echo "Latest version: ${VERSION}"
fi


ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) echo "FATAL: Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

# Download and extract
URL="https://github.com/${UPSTREAM_REPO}/releases/download/v${VERSION}/llama-swap_${VERSION}_linux_${ARCH}.tar.gz"
echo "=== Downloading llama-swap v${VERSION} ==="
echo "URL: $URL"
curl -fSL -o /tmp/llama-swap.tar.gz "$URL"
tar -xzf /tmp/llama-swap.tar.gz -C /install/bin/
rm /tmp/llama-swap.tar.gz

# Validate
if [ ! -x "/install/bin/llama-swap" ]; then
    echo "FATAL: llama-swap binary not found or not executable" >&2
    ls -la /install/bin/ >&2
    exit 1
fi

echo "$VERSION" > /install/llama-swap-version

echo "=== llama-swap v${VERSION} installed ==="
ls -la /install/bin/llama-swap

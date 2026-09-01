#!/bin/bash
# Install vllm-wrapper - build from llama-swap source
# Usage: ./install-vllm-wrapper.sh [version]
#   version: full commit hash, release version (e.g. "170"/"v170") or "latest" (default)
#
# vllm-wrapper is not part of the llama-swap release archives so it is always
# compiled from source, from the same revision the llama-swap binary was (or
# would have been) released from -- an upstream release tag when the commit
# matches one, otherwise the commit itself. This mirrors install-llama-swap.sh
# so both binaries come from the same revision.
#
# LLAMA_SWAP_REPO (env) - git URL to build from. Defaults to upstream;
# build-image.sh sets it to the running repository.
set -e

VERSION="${1:-latest}"
UPSTREAM_REPO="mostlygeek/llama-swap"
LLAMA_SWAP_REPO="${LLAMA_SWAP_REPO:-https://github.com/${UPSTREAM_REPO}.git}"
SRC=/src/llama-swap

mkdir -p /install/bin

REF=""

# If a full commit hash is given, check whether it matches an upstream
# release tag; if so, build from that tag (readable version string), else
# build from the commit itself directly -- the normal case on a fork, which
# has no release tags to check against.
if echo "${VERSION}" | grep -qE '^[0-9a-f]{40}$'; then
    echo "=== Resolving commit ${VERSION:0:7} against upstream releases ==="
    TAG=$(git ls-remote --tags "https://github.com/${UPSTREAM_REPO}.git" 2>/dev/null \
        | grep "^${VERSION}" | sed 's|.*refs/tags/||' | grep -v '\^{}' | head -1)
    if [ -n "${TAG}" ]; then
        echo "Resolved to upstream tag: ${TAG}"
        REF="${TAG}"
    else
        echo "No upstream release matches commit ${VERSION:0:7}; building from commit"
        REF="${VERSION}"
    fi
fi

if [ -z "${REF}" ]; then
    # Strip leading 'v' prefix so both "198" and "v198" work
    VERSION="${VERSION#v}"

    # Resolve "latest" to the tag of the most recent release
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

    REF="v${VERSION}"
fi

echo "=== Cloning ${LLAMA_SWAP_REPO} @ ${REF} ==="
git clone --filter=blob:none --no-checkout "${LLAMA_SWAP_REPO}" "${SRC}"
git -C "${SRC}" checkout --detach "${REF}"

if [ ! -d "${SRC}/cmd/vllm-wrapper" ]; then
    echo "FATAL: ${REF} has no cmd/vllm-wrapper (added in v243), pin LS_VERSION to v243 or newer" >&2
    exit 1
fi

echo "=== Building vllm-wrapper ==="
cd "${SRC}"
CGO_ENABLED=0 go build -trimpath -o /install/bin/vllm-wrapper ./cmd/vllm-wrapper

# Validate
if [ ! -x "/install/bin/vllm-wrapper" ]; then
    echo "FATAL: vllm-wrapper binary not found or not executable" >&2
    ls -la /install/bin/ >&2
    exit 1
fi

echo "=== vllm-wrapper (${REF}) installed ==="
ls -la /install/bin/vllm-wrapper

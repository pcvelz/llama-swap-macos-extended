#!/usr/bin/env bash
#
# preflight.sh — run locally what GitHub Actions runs remotely, before the commit.
#
# WHY THIS EXISTS
# ---------------
# On 2026-07-27 both Linux CI and Windows CI went red on main. The failure was a
# data race in internal/router's pingWriter tests: `go test` passes, `go test
# -race` does not, and `make test-all` (what BOTH CI lanes run) uses -race. The
# commit had been verified locally with a plain `go test ./internal/...`, so the
# gap was invisible until GitHub mailed the failure. CI had in fact been red
# since the commit before it, for the same reason.
#
# The lesson is not "remember to add -race". It is that local verification and CI
# were running different commands, so local green carried no information. This
# script closes that: it is derived from .github/workflows/*.yml and runs the
# same commands, with the same path scoping, so a green preflight is a real
# prediction about CI rather than a weaker check that happens to pass.
#
# WHAT IT MIRRORS (source of truth: .github/workflows/)
#   go-ci.yml          gofmt -l . must be empty; make simple-responder; make test-all
#   go-ci-windows.yml  make test-all  (same command; the OS-specific half cannot
#                      run here, but every failure seen so far reproduced under
#                      -race on any platform)
#   tray-ci.yml        go test ./internal/tray/... ./internal/menubar/... ./internal/config/...
#                      cross-builds for linux/amd64, linux/arm64, windows/amd64
#                      macOS: cd macos-menu && swift build -c release
#   config-schema.yml  go test ./internal/config/ -run TestConfig_ExampleMatchesSchema
#   ui-tests.yml       make test-ui
#   workflow hygiene   not a CI mirror — this repo is a fork of
#                      mostlygeek/llama-swap, so .github/workflows/ and docker/
#                      changes get: actionlint; a grep for ghcr.io/mostlygeek or
#                      mostlygeek/llama-swap hardcoded as a push/cache/clone
#                      target (the only allowed form is one documented
#                      UPSTREAM_REPO="mostlygeek/llama-swap" fallback line per
#                      script); a remote check (needs gh) that the workflows
#                      this repo keeps disabled are actually disabled on
#                      GitHub and every active workflow's latest run on main is
#                      green; and, when docker/unified/** or its workflow
#                      changed, `bash -n` on those scripts plus a --help smoke
#                      test of build-image.sh
#
# SCOPING
# Each CI workflow has `paths:` filters, so a docs-only commit triggers nothing.
# This script applies the same idea to the CHANGED FILES, keeping a typical
# commit fast while still running every lane the change can actually break.
# Pass --all to force every lane regardless of what changed.
#
# HONEST LIMITATION
# This tests the WORKING TREE, not the staged index — matching what a developer
# is about to commit in the common case, but diverging under partial staging
# (`git add -p`, or a file staged and then edited again). That case is detected
# and reported rather than silently ignored: see the partial-staging warning.
# There is no stash-based isolation on purpose; stashing to run a check is how
# uncommitted work gets lost.
#
# EXIT: 0 = every selected lane passed. 1 = at least one failed (details above).
#       Skipped lanes (missing toolchain) are reported and never fail the run.
#
# Usage:
#   scripts/preflight.sh            # scope to changed files (pre-commit default)
#   scripts/preflight.sh --all      # run every lane
#   scripts/preflight.sh --quick    # skip the slow -race suite (NOT a CI predictor)
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1

# Workflows this fork keeps fully disabled on GitHub rather than fixing up to
# run here (used by the "workflow hygiene" lane's remote-state check).
DISABLED_WORKFLOWS=(closeinactive.yml containers.yml release.yml)

MODE_ALL=0
MODE_QUICK=0
for arg in "$@"; do
    case "$arg" in
        --all)   MODE_ALL=1 ;;
        --quick) MODE_QUICK=1 ;;
        -h|--help) sed -n '2,63p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) echo "unknown argument: $arg" >&2; exit 2 ;;
    esac
done

FAILED=()
PASSED=()
SKIPPED=()

hdr()  { printf '\n\033[1m── %s ──\033[0m\n' "$1"; }
pass() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; PASSED+=("$1"); }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; FAILED+=("$1"); }
skip() { printf '  \033[33mSKIP\033[0m  %s\n' "$1"; SKIPPED+=("$1"); }

# run <label> <command...> — echoes output only on failure, so a green run stays
# readable and a red one shows everything needed to act.
run() {
    local label="$1"; shift
    local out
    if out=$("$@" 2>&1); then
        pass "$label"
    else
        fail "$label"
        printf '%s\n' "$out" | sed 's/^/        /'
    fi
}

# ---------------------------------------------------------------------------
# Which files changed?
# ---------------------------------------------------------------------------
# Union of staged and unstaged changes plus untracked files: a new _test.go that
# is staged but never compiled locally is exactly the kind of thing that reaches
# CI red, so untracked files must count.
changed_files() {
    {
        git diff --name-only --cached
        git diff --name-only
        git ls-files --others --exclude-standard
    } | sort -u
}

CHANGED="$(changed_files)"

if [[ $MODE_ALL -eq 0 && -z "$CHANGED" ]]; then
    echo "preflight: no changes detected — nothing to check."
    exit 0
fi

# touched <regex> — does any changed path match?
touched() {
    [[ $MODE_ALL -eq 1 ]] && return 0
    printf '%s\n' "$CHANGED" | grep -qE "$1"
}

# CI's go lanes filter on **/*.go excluding cmd/**, plus go.mod/go.sum/Makefile.
GO_PATHS='(^|/)[^/]+\.go$|^go\.(mod|sum)$|^Makefile$'
go_touched() {
    [[ $MODE_ALL -eq 1 ]] && return 0
    printf '%s\n' "$CHANGED" | grep -E "$GO_PATHS" | grep -qv '^cmd/'
}

echo "=== llama-swap preflight ==="
if [[ $MODE_ALL -eq 1 ]]; then
    echo "scope: ALL lanes (--all)"
else
    echo "scope: $(printf '%s\n' "$CHANGED" | grep -c .) changed path(s)"
fi

# ---------------------------------------------------------------------------
# Partial-staging warning
# ---------------------------------------------------------------------------
# A file that is BOTH staged and modified means the tree being tested is not the
# content being committed. Warn loudly; do not fail — the developer may know.
BOTH=$(comm -12 <(git diff --name-only --cached | sort -u) <(git diff --name-only | sort -u))
if [[ -n "$BOTH" ]]; then
    printf '\n\033[33mWARNING\033[0m  these files are staged AND further modified —\n'
    printf '         preflight tests the working tree, so it is NOT checking\n'
    printf '         exactly what would be committed:\n'
    printf '%s\n' "$BOTH" | sed 's/^/           /'
fi

# ---------------------------------------------------------------------------
# Lane: gofmt  (go-ci.yml "Check Formatting")
# ---------------------------------------------------------------------------
if go_touched; then
    hdr "gofmt (go-ci.yml)"
    if ! command -v gofmt >/dev/null 2>&1; then
        skip "gofmt — Go toolchain not installed"
    else
        UNFMT=$(gofmt -l . 2>/dev/null)
        if [[ -z "$UNFMT" ]]; then
            pass "gofmt -l . is empty"
        else
            fail "gofmt -l . reported unformatted files"
            printf '%s\n' "$UNFMT" | sed 's/^/        /'
            printf '        fix: gofmt -w %s\n' "$(printf '%s' "$UNFMT" | tr '\n' ' ')"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# Lane: build + vet  (not a CI step, but it fails faster than the test suite
# and every CI failure implies it)
# ---------------------------------------------------------------------------
if go_touched; then
    hdr "build + vet"
    if ! command -v go >/dev/null 2>&1; then
        skip "go build/vet — Go toolchain not installed"
    else
        run "go build ./..." go build ./...
        run "go vet ./internal/..." go vet ./internal/...
    fi
fi

# ---------------------------------------------------------------------------
# Lane: the race suite  (go-ci.yml + go-ci-windows.yml "Test all")
# ---------------------------------------------------------------------------
# THE lane that matters. `make test-all` is `go test -race -count=1
# ./internal/...`; running it without -race is what let the 2026-07-27 failure
# through. It needs cmd/simple-responder built (CI caches it) for the process
# swapping tests.
if go_touched; then
    hdr "make test-all — go test -race (go-ci.yml, go-ci-windows.yml)"
    if ! command -v go >/dev/null 2>&1; then
        skip "make test-all — Go toolchain not installed"
    elif [[ $MODE_QUICK -eq 1 ]]; then
        skip "make test-all — --quick given (this does NOT predict CI)"
    else
        if [[ ! -x build/simple-responder ]] && ! ls build/simple-responder* >/dev/null 2>&1; then
            run "make simple-responder" make simple-responder
        fi
        echo "        (this is the slow one — full -race suite, several minutes)"
        run "make test-all" make test-all
    fi
fi

# ---------------------------------------------------------------------------
# Lane: config schema  (config-schema.yml)
# ---------------------------------------------------------------------------
if touched '^(config-schema\.json|config\.example\.yaml|internal/config/)'; then
    hdr "config schema (config-schema.yml)"
    if ! command -v go >/dev/null 2>&1; then
        skip "config schema — Go toolchain not installed"
    else
        run "config.example.yaml matches config-schema.json" \
            go test ./internal/config/ -run TestConfig_ExampleMatchesSchema
    fi
fi

# ---------------------------------------------------------------------------
# Lane: tray + menubar  (tray-ci.yml)
# ---------------------------------------------------------------------------
# tray-ci cross-builds on three platforms; the cross-builds are the part most
# likely to break from a macOS-only dev box, so they run even though the local
# `go build ./...` already passed for darwin.
if touched '^(internal/(tray|menubar|config)/|cmd/llama-swap-tray/|\.github/workflows/tray-ci\.yml)'; then
    hdr "tray + menubar (tray-ci.yml)"
    if ! command -v go >/dev/null 2>&1; then
        skip "tray lane — Go toolchain not installed"
    else
        run "go test ./internal/tray/... ./internal/menubar/... ./internal/config/..." \
            go test -count=1 ./internal/tray/... ./internal/menubar/... ./internal/config/...
        # CGO_ENABLED=0 for the cross-builds: the tray uses cgo natively, and CI
        # builds these targets without a cross C toolchain.
        for target in linux/amd64 linux/arm64 windows/amd64; do
            _goos="${target%/*}"; _goarch="${target#*/}"
            if out=$(CGO_ENABLED=0 GOOS="$_goos" GOARCH="$_goarch" \
                     go build -o /dev/null ./cmd/llama-swap-tray 2>&1); then
                pass "cross-build $target"
            else
                fail "cross-build $target"
                printf '%s\n' "$out" | sed 's/^/        /'
            fi
        done
    fi
fi

# ---------------------------------------------------------------------------
# Lane: macOS Swift menu-bar helper  (tray-ci.yml, macos-latest job)
# ---------------------------------------------------------------------------
# CI builds RELEASE. A debug build can succeed where release fails, so match it.
if touched '^macos-menu/'; then
    hdr "Swift menu-bar helper (tray-ci.yml macOS job)"
    if [[ "$(uname -s)" != "Darwin" ]]; then
        skip "swift build — not macOS"
    elif ! command -v swift >/dev/null 2>&1; then
        skip "swift build — Swift toolchain not installed"
    else
        run "swift build -c release (macos-menu)" \
            swift build --package-path macos-menu -c release
        # Not a CI step, but the helper has a test target and it is cheap.
        run "swift test (macos-menu)" swift test --package-path macos-menu
    fi
fi

# ---------------------------------------------------------------------------
# Lane: UI  (ui-tests.yml)
# ---------------------------------------------------------------------------
if touched '^ui-svelte/'; then
    hdr "UI tests (ui-tests.yml)"
    if ! command -v npm >/dev/null 2>&1; then
        skip "make test-ui — npm not installed"
    else
        run "make test-ui" make test-ui
    fi
fi

# ---------------------------------------------------------------------------
# Lane: workflow hygiene  (not a CI mirror — this repo is a fork)
# ---------------------------------------------------------------------------
# Fork-specific class of failure: a workflow or docker script that hardcodes
# the upstream owner as a push/cache/clone target doesn't work here (permission
# denied pushing to someone else's ghcr namespace, wrong clone source, etc).
if touched '^(\.github/workflows/|docker/)'; then
    hdr "workflow hygiene (fork of mostlygeek/llama-swap)"

    if ! command -v actionlint >/dev/null 2>&1; then
        skip "actionlint — not installed (brew install actionlint)"
    else
        run "actionlint .github/workflows/*.yml" actionlint .github/workflows/*.yml
    fi

    # Every ghcr.io/mostlygeek or mostlygeek/llama-swap reference under
    # .github/workflows/ or docker/ must be one of: a comment; a
    # `github.repository == 'mostlygeek/llama-swap'` comparison (a legitimate
    # skip-on-fork guard, not a push/clone target); a shell overridable
    # default (`${VAR:-mostlygeek/llama-swap}` / `${VAR:-ghcr.io/mostlygeek/...}`);
    # a Dockerfile/Containerfile `ARG NAME=mostlygeek/llama-swap` default; or
    # the single documented local-build fallback
    # `UPSTREAM_REPO="mostlygeek/llama-swap"`. Everything else that names the
    # upstream owner as a push/cache/clone/download target is a FAIL.
    HARDCODE_HITS=""
    while IFS= read -r -d '' f; do
        hits=$(grep -nE 'ghcr\.io/mostlygeek|mostlygeek/llama-swap' "$f" 2>/dev/null \
            | grep -vE '^[0-9]+:[[:space:]]*#' \
            | grep -vE "github\.repository[[:space:]]*==" \
            | grep -vE ':-mostlygeek/llama-swap|:-ghcr\.io/mostlygeek' \
            | grep -vE '^[0-9]+:[[:space:]]*ARG[[:space:]]+[A-Za-z_][A-Za-z0-9_]*=mostlygeek/llama-swap' \
            | grep -vE '^[0-9]+:UPSTREAM_REPO="mostlygeek/llama-swap"$')
        if [[ -n "$hits" ]]; then
            HARDCODE_HITS+="${f#"$REPO"/}:"$'\n'"$hits"$'\n'
        fi
    done < <(find .github/workflows docker -type f \
        \( -name '*.yml' -o -name '*.yaml' -o -name '*.sh' -o -iname '*containerfile*' -o -iname 'dockerfile*' \) \
        -print0 2>/dev/null)
    if [[ -z "$HARDCODE_HITS" ]]; then
        pass "no hardcoded upstream-owner push/cache/clone targets"
    else
        fail "hardcoded upstream-owner (mostlygeek) push/cache/clone target"
        printf '%s\n' "$HARDCODE_HITS" | sed 's/^/        /'
        printf '        fix: derive from GITHUB_REPOSITORY / the checked-out source;\n'
        printf '        allowed forms: github.repository == comparisons, ${VAR:-mostlygeek/...}\n'
        printf '        defaults, ARG NAME=mostlygeek/llama-swap, and one UPSTREAM_REPO=... fallback line\n'
    fi

    # Remote workflow state — needs gh + network; SKIP otherwise or under --quick.
    if [[ $MODE_QUICK -eq 1 ]]; then
        skip "remote workflow state — --quick given"
    elif ! command -v gh >/dev/null 2>&1; then
        skip "remote workflow state — gh not installed"
    else
        FORK_REPO="$(git remote get-url origin 2>/dev/null \
            | sed -E 's#^git@github\.com:##; s#^https?://github\.com/##; s#\.git$##')"
        WF_JSON=""
        [[ -n "$FORK_REPO" ]] && WF_JSON="$(gh api "repos/${FORK_REPO}/actions/workflows" 2>/dev/null)"
        if [[ -z "$FORK_REPO" || -z "$WF_JSON" ]]; then
            skip "remote workflow state — gh api unavailable (offline?)"
        else
            for name in "${DISABLED_WORKFLOWS[@]}"; do
                state=$(printf '%s' "$WF_JSON" | jq -r --arg p ".github/workflows/${name}" \
                    '.workflows[] | select(.path == $p) | .state' 2>/dev/null)
                if [[ -z "$state" ]]; then
                    skip "$name — not found in remote workflow list"
                elif [[ "$state" == "disabled_fork" || "$state" == "disabled_manually" ]]; then
                    pass "$name: remote state is $state"
                else
                    fail "$name: expected disabled but remote state is '$state'"
                fi
            done

            while IFS=$'\t' read -r wf_name wf_path; do
                [[ -n "$wf_path" ]] || continue
                run_json=$(gh run list -R "$FORK_REPO" --workflow "$(basename "$wf_path")" \
                    --branch main --status completed --limit 1 --json conclusion,url 2>/dev/null) || run_json="[]"
                conclusion=$(printf '%s' "$run_json" | jq -r '.[0].conclusion // empty' 2>/dev/null)
                url=$(printf '%s' "$run_json" | jq -r '.[0].url // empty' 2>/dev/null)
                if [[ -z "$conclusion" ]]; then
                    pass "$wf_name: active, no completed runs on main yet"
                elif [[ "$conclusion" == "success" ]]; then
                    pass "$wf_name: latest run on main is green"
                elif [[ $MODE_ALL -eq 1 ]]; then
                    # Release-time (--all): red CI blocks. Per-commit it must
                    # not, or the commit that fixes the red run can never land.
                    fail "$wf_name: latest run on main concluded '$conclusion' — $url"
                else
                    skip "$wf_name: latest run on main is '$conclusion' (red; blocks under --all) — $url"
                fi
            done < <(printf '%s' "$WF_JSON" | jq -r '.workflows[] | select(.state == "active") | "\(.name)\t\(.path)"' 2>/dev/null)
        fi
    fi

    # unified-docker sanity — only when its own scripts/workflow changed.
    if touched '^(docker/unified/|\.github/workflows/unified-docker.*\.ya?ml)'; then
        for f in docker/unified/*.sh; do
            [[ -f "$f" ]] || continue
            run "bash -n $f" bash -n "$f"
        done
        if [[ -x docker/unified/build-image.sh ]]; then
            run "docker/unified/build-image.sh --help" docker/unified/build-image.sh --help
        fi
    fi
fi

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------
printf '\n=== preflight: %d passed, %d failed, %d skipped ===\n' \
    "${#PASSED[@]}" "${#FAILED[@]}" "${#SKIPPED[@]}"

if [[ ${#SKIPPED[@]} -gt 0 ]]; then
    printf 'skipped (NOT verified — CI still runs these):\n'
    printf '  - %s\n' "${SKIPPED[@]}"
fi

if [[ ${#FAILED[@]} -gt 0 ]]; then
    printf '\nfailed:\n'
    printf '  - %s\n' "${FAILED[@]}"
    printf '\nCI runs these same commands; expect it to fail too.\n'
    exit 1
fi

exit 0

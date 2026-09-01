#!/usr/bin/env bash
# test-ci-await.sh - exercises scripts/ci-await.sh against a fake `gh` shim.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/../ci-await.sh"
FAIL=0

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

mk_fake_gh() {
  local dir="$1"
  cat > "$dir/gh" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
DIR="${FAKE_GH_DIR:?FAKE_GH_DIR not set}"

case "${1:-}" in
  run)
    case "${2:-}" in
      list)
        count_file="$DIR/run_list_call_count"
        count=0
        [ -f "$count_file" ] && count="$(cat "$count_file")"
        count=$((count + 1))
        echo "$count" > "$count_file"
        resp="$DIR/run_list_${count}.json"
        if [ ! -f "$resp" ]; then
          last="$(ls "$DIR"/run_list_*.json 2>/dev/null | sort -V | tail -1)"
          resp="$last"
        fi
        cat "$resp"
        ;;
      view)
        id="${3:-}"
        if printf '%s\n' "$@" | grep -q -- '--log-failed'; then
          cat "$DIR/log_failed_${id}.txt" 2>/dev/null || true
        else
          cat "$DIR/run_view_${id}.json" 2>/dev/null || echo '{"jobs":[]}'
        fi
        ;;
      *)
        echo '[]'
        ;;
    esac
    ;;
  api)
    cat "$DIR/api_workflows.json" 2>/dev/null || echo '{"workflows":[]}'
    ;;
  *)
    echo '[]'
    ;;
esac
SHIM
  chmod +x "$dir/gh"
}

assert_contains() {
  local haystack="$1" needle="$2" label="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  ok: $label"
  else
    echo "  FAIL: $label (expected to find: $needle)"
    FAIL=1
  fi
}

assert_eq() {
  local actual="$1" expected="$2" label="$3"
  if [ "$actual" = "$expected" ]; then
    echo "  ok: $label"
  else
    echo "  FAIL: $label (expected [$expected], got [$actual])"
    FAIL=1
  fi
}

# --- Test 1: zero runs -> NO-RUNS, exit 0 -----------------------------------

echo "Test 1: zero runs discovered"
T1="$TMP_ROOT/t1"
mkdir -p "$T1"
mk_fake_gh "$T1"
echo '[]' > "$T1/run_list_1.json"

set +e
out="$(FAKE_GH_DIR="$T1" CI_AWAIT_GH="$T1/gh" CI_AWAIT_POLL=1 CI_AWAIT_APPEAR_WAIT=1 \
  "$SCRIPT" deadbeef --repo pcvelz/llama-swap-macos-extended 2>&1)"
code=$?
set -e
assert_eq "$code" "0" "exit code 0"
assert_contains "$out" "NO-RUNS" "prints NO-RUNS"

# --- Test 2: in_progress then success on second poll -> exit 0, CI GREEN ---

echo "Test 2: in_progress -> success"
T2="$TMP_ROOT/t2"
mkdir -p "$T2"
mk_fake_gh "$T2"
cat > "$T2/run_list_1.json" <<'JSON'
[{"databaseId":101,"name":"go-ci","workflowName":"go-ci","status":"in_progress","conclusion":null,"url":"https://example/101","startedAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:00:00Z"}]
JSON
cp "$T2/run_list_1.json" "$T2/run_list_2.json"
cat > "$T2/run_list_3.json" <<'JSON'
[{"databaseId":101,"name":"go-ci","workflowName":"go-ci","status":"completed","conclusion":"success","url":"https://example/101","startedAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:01:05Z"}]
JSON

set +e
out="$(FAKE_GH_DIR="$T2" CI_AWAIT_GH="$T2/gh" CI_AWAIT_POLL=0 CI_AWAIT_APPEAR_WAIT=5 \
  "$SCRIPT" cafef00d --repo pcvelz/llama-swap-macos-extended 2>&1)"
code=$?
set -e
assert_eq "$code" "0" "exit code 0"
assert_contains "$out" "CI GREEN for cafef00d" "prints CI GREEN"
assert_contains "$out" "go-ci" "table lists workflow name"

# --- Test 3: one failure -> exit 1, error lines from fake log appear -------

echo "Test 3: one failing run"
T3="$TMP_ROOT/t3"
mkdir -p "$T3"
mk_fake_gh "$T3"
cat > "$T3/run_list_1.json" <<'JSON'
[{"databaseId":202,"name":"go-ci","workflowName":"go-ci","status":"completed","conclusion":"failure","url":"https://example/202","startedAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:02:00Z"}]
JSON
cat > "$T3/run_view_202.json" <<'JSON'
{"jobs":[{"conclusion":"failure","steps":[{"name":"Run tests","conclusion":"failure"},{"name":"Build","conclusion":"success"}]}]}
JSON
cat > "$T3/log_failed_202.txt" <<'LOG'
some noisy line
##[error]TestSomething failed
FAIL: TestSomething (0.01s)
process exited with exit code 1
more noise
LOG

set +e
out="$(FAKE_GH_DIR="$T3" CI_AWAIT_GH="$T3/gh" CI_AWAIT_POLL=0 CI_AWAIT_APPEAR_WAIT=5 \
  "$SCRIPT" badc0de --repo pcvelz/llama-swap-macos-extended 2>&1)"
code=$?
set -e
assert_eq "$code" "1" "exit code 1"
assert_contains "$out" "Run tests" "prints failing step name"
assert_contains "$out" "##[error]TestSomething failed" "prints error line from fake log"

# --- Test 4: upstream origin -> abort, exit 2 -------------------------------

echo "Test 4: upstream repo aborts"
T4="$TMP_ROOT/t4"
mkdir -p "$T4"
mk_fake_gh "$T4"

set +e
out="$(FAKE_GH_DIR="$T4" CI_AWAIT_GH="$T4/gh" CI_AWAIT_POLL=0 CI_AWAIT_APPEAR_WAIT=1 \
  "$SCRIPT" deadbeef --repo mostlygeek/llama-swap 2>&1)"
code=$?
set -e
assert_eq "$code" "2" "exit code 2"
assert_contains "$out" "ABORT" "prints ABORT"

echo ""
if [ "$FAIL" -eq 0 ]; then
  echo "ALL TESTS PASSED"
  exit 0
else
  echo "TESTS FAILED"
  exit 1
fi

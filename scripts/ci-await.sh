#!/usr/bin/env bash
#
# ci-await.sh - wait for GitHub Actions triggered by a commit (or the latest
# scheduled/cron runs of every active workflow) to go green, on the FORK
# (pcvelz/llama-swap-macos-extended) only. Never watches or acts on the
# upstream repo (mostlygeek/llama-swap) - if the resolved repo is upstream,
# this script aborts.
#
# Usage:
#   scripts/ci-await.sh [<sha>] [--timeout <sec>] [--repo <owner/name>]
#   scripts/ci-await.sh --latest-scheduled [--repo <owner/name>]
#
# Default sha:    git rev-parse HEAD
# Default repo:   derived from `git remote get-url origin`
# Default timeout: 1800 seconds
#
# Behavior:
#   - Discovers runs for the commit via
#       gh run list -R <repo> --commit <sha> --json databaseId,name,status,conclusion,url,workflowName
#     Workflows use `paths:` filters, so it is NORMAL for zero runs to be
#     triggered by a given commit. This script waits up to
#     CI_AWAIT_APPEAR_WAIT seconds (default 90) for runs to appear (GitHub
#     registers them with a delay). If none appear, prints:
#       NO-RUNS: no workflow matched this commit's paths
#     and exits 0.
#   - Otherwise polls every CI_AWAIT_POLL seconds (default 20) until every
#     discovered run is `completed`, or the timeout elapses (exit 3, printing
#     what is still running). Prints one status line per poll, only when
#     something changed since the last poll (no spam).
#   - On completion, prints a table: workflow, conclusion, duration, url.
#     If any conclusion is not `success` or `skipped`: for each failing run,
#     prints its failing step names and the last ~40 lines of
#       gh run view <id> -R <repo> --log-failed
#     filtered to actual error lines (##[error], ERROR, FAIL, exit code),
#     then exits 1. If everything is green: prints `CI GREEN for <sha>` and
#     exits 0.
#   - --latest-scheduled: instead of watching a commit, reports the most
#     recent COMPLETED run of every ACTIVE workflow on main (discovered via
#     `gh api repos/<repo>/actions/workflows` filtered to state==active, then
#     `gh run list --workflow <id> --limit 1`). Same table format. Exits 1 if
#     any of those latest runs is red (not success/skipped), else 0. This is
#     the daily-cron cascade check - it does not wait/poll, it is a snapshot.
#   - No `timeout(1)` dependency (macOS default has none). No em-dashes in
#     any output.
#
# Env overrides:
#   CI_AWAIT_POLL         poll interval in seconds (default 20)
#   CI_AWAIT_APPEAR_WAIT  seconds to wait for runs to first appear (default 90)
#   CI_AWAIT_GH           override the `gh` binary/shim used (default: gh)
#
# Exit codes:
#   0  all discovered runs green (or NO-RUNS case)
#   1  one or more runs completed with a non-success/skipped conclusion
#   2  repo resolved to upstream (mostlygeek/llama-swap) - aborted, unsafe
#   3  timeout hit before all runs completed
#
set -euo pipefail

GH_BIN="${CI_AWAIT_GH:-gh}"
POLL_INTERVAL="${CI_AWAIT_POLL:-20}"
APPEAR_WAIT="${CI_AWAIT_APPEAR_WAIT:-90}"
TIMEOUT=1800
REPO=""
SHA=""
LATEST_SCHEDULED=0

usage() {
  echo "Usage: $0 [<sha>] [--timeout <sec>] [--repo <owner/name>]" >&2
  echo "       $0 --latest-scheduled [--repo <owner/name>]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --timeout)
      TIMEOUT="$2"
      shift 2
      ;;
    --repo)
      REPO="$2"
      shift 2
      ;;
    --latest-scheduled)
      LATEST_SCHEDULED=1
      shift
      ;;
    -h|--help)
      usage
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage
      ;;
    *)
      if [ -z "$SHA" ]; then
        SHA="$1"
      else
        echo "Unexpected argument: $1" >&2
        usage
      fi
      shift
      ;;
  esac
done

# Resolve repo from origin if not given.
if [ -z "$REPO" ]; then
  origin_url="$(git remote get-url origin 2>/dev/null || true)"
  if [ -z "$origin_url" ]; then
    echo "ERROR: could not resolve origin remote and no --repo given" >&2
    exit 2
  fi
  # Strip protocol/host prefix and .git suffix, keep owner/name.
  REPO="$(printf '%s' "$origin_url" \
    | sed -E 's#^git@github\.com:##; s#^https?://github\.com/##; s#\.git$##')"
fi

case "$REPO" in
  mostlygeek/llama-swap|mostlygeek/llama-swap.git)
    echo "ABORT: repo resolved to upstream (mostlygeek/llama-swap) - refusing to watch/act on upstream" >&2
    exit 2
    ;;
esac

if [ "$LATEST_SCHEDULED" -eq 1 ]; then
  SHA=""
else
  if [ -z "$SHA" ]; then
    SHA="$(git rev-parse HEAD)"
  fi
fi

# --- helpers ---------------------------------------------------------------

# Print a table of runs. Expects a JSON array on stdin with fields:
# workflowName/name, conclusion, url, and (optionally) startedAt/updatedAt
# for duration, or databaseId to look duration up separately.
print_table() {
  local json="$1"
  printf '%-30s %-12s %-10s %s\n' "WORKFLOW" "CONCLUSION" "DURATION" "URL"
  printf '%s\n' "$json" | "$JQ_BIN" -r '
    .[] | [
      (.workflowName // .name // "unknown"),
      (.conclusion // "n/a"),
      (.durationLabel // "n/a"),
      (.url // "")
    ] | @tsv' | while IFS=$'\t' read -r wf concl dur url; do
    printf '%-30s %-12s %-10s %s\n' "$wf" "$concl" "$dur" "$url"
  done
}

# Compute a human duration label (Ns / Nm Ns) from ISO8601 start/end.
duration_label() {
  local start="$1" end="$2"
  if [ -z "$start" ] || [ -z "$end" ] || [ "$start" = "null" ] || [ "$end" = "null" ]; then
    echo "n/a"
    return
  fi
  local start_epoch end_epoch
  start_epoch="$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$start" "+%s" 2>/dev/null || date -d "$start" "+%s" 2>/dev/null || echo "")"
  end_epoch="$(date -j -f "%Y-%m-%dT%H:%M:%SZ" "$end" "+%s" 2>/dev/null || date -d "$end" "+%s" 2>/dev/null || echo "")"
  if [ -z "$start_epoch" ] || [ -z "$end_epoch" ]; then
    echo "n/a"
    return
  fi
  local secs=$((end_epoch - start_epoch))
  if [ "$secs" -lt 0 ]; then
    echo "n/a"
    return
  fi
  if [ "$secs" -ge 60 ]; then
    echo "$((secs / 60))m $((secs % 60))s"
  else
    echo "${secs}s"
  fi
}

JQ_BIN="jq"

print_failures_and_exit() {
  local runs_json="$1"
  local any_red=0
  local ids
  ids="$(printf '%s' "$runs_json" | "$JQ_BIN" -r '.[] | select((.conclusion != "success") and (.conclusion != "skipped")) | .databaseId')"
  if [ -n "$ids" ]; then
    any_red=1
  fi
  while IFS= read -r id; do
    [ -z "$id" ] && continue
    local wf
    wf="$(printf '%s' "$runs_json" | "$JQ_BIN" -r --arg id "$id" '.[] | select(.databaseId == ($id | tonumber)) | (.workflowName // .name)')"
    echo ""
    echo "=== FAILED: $wf (run $id) ==="
    echo "--- failing steps ---"
    "$GH_BIN" run view "$id" -R "$REPO" --json jobs 2>/dev/null \
      | "$JQ_BIN" -r '.jobs[]? | select(.conclusion != "success" and .conclusion != "skipped") | .steps[]? | select(.conclusion != "success" and .conclusion != "skipped" and .conclusion != null) | .name' \
      || true
    echo "--- last error lines (gh run view --log-failed) ---"
    "$GH_BIN" run view "$id" -R "$REPO" --log-failed 2>/dev/null \
      | grep -E '##\[error\]|ERROR|FAIL|exit code' \
      | tail -n 40 \
      || true
  done <<< "$ids"
  if [ "$any_red" -eq 1 ]; then
    return 1
  fi
  return 0
}

# --- --latest-scheduled mode -----------------------------------------------

if [ "$LATEST_SCHEDULED" -eq 1 ]; then
  workflows_json="$("$GH_BIN" api "repos/$REPO/actions/workflows" --paginate 2>/dev/null || echo '{"workflows":[]}')"
  active_ids="$(printf '%s' "$workflows_json" | "$JQ_BIN" -r '.workflows[]? | select(.state == "active") | .id')"

  if [ -z "$active_ids" ]; then
    echo "NO-RUNS: no active workflows found in $REPO"
    exit 0
  fi

  all_runs="[]"
  while IFS= read -r wf_id; do
    [ -z "$wf_id" ] && continue
    run_json="$("$GH_BIN" run list -R "$REPO" --workflow "$wf_id" --status completed --limit 1 --json databaseId,name,workflowName,status,conclusion,url,startedAt,updatedAt 2>/dev/null || echo '[]')"
    all_runs="$(printf '%s\n%s' "$all_runs" "$run_json" | "$JQ_BIN" -s 'add')"
  done <<< "$active_ids"

  # Annotate duration labels.
  enriched="[]"
  count="$(printf '%s' "$all_runs" | "$JQ_BIN" 'length')"
  i=0
  while [ "$i" -lt "$count" ]; do
    row="$(printf '%s' "$all_runs" | "$JQ_BIN" ".[$i]")"
    start="$(printf '%s' "$row" | "$JQ_BIN" -r '.startedAt // "null"')"
    end="$(printf '%s' "$row" | "$JQ_BIN" -r '.updatedAt // "null"')"
    dur="$(duration_label "$start" "$end")"
    row="$(printf '%s' "$row" | "$JQ_BIN" --arg d "$dur" '. + {durationLabel: $d}')"
    enriched="$(printf '%s\n%s' "$enriched" "$row" | "$JQ_BIN" -s '.[0] + [.[1]]')"
    i=$((i + 1))
  done

  echo "Latest scheduled/active-workflow runs for $REPO:"
  print_table "$enriched"

  if print_failures_and_exit "$enriched"; then
    echo ""
    echo "CI GREEN (latest scheduled) for $REPO"
    exit 0
  else
    exit 1
  fi
fi

# --- commit-watch mode -------------------------------------------------------

echo "Watching CI for $REPO @ $SHA (timeout ${TIMEOUT}s, poll ${POLL_INTERVAL}s)"

elapsed=0
runs_json="[]"
while [ "$elapsed" -lt "$APPEAR_WAIT" ]; do
  runs_json="$("$GH_BIN" run list -R "$REPO" --commit "$SHA" --json databaseId,name,workflowName,status,conclusion,url,startedAt,updatedAt 2>/dev/null || echo '[]')"
  count="$(printf '%s' "$runs_json" | "$JQ_BIN" 'length')"
  if [ "$count" -gt 0 ]; then
    break
  fi
  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
done

count="$(printf '%s' "$runs_json" | "$JQ_BIN" 'length')"
if [ "$count" -eq 0 ]; then
  echo "NO-RUNS: no workflow matched this commit's paths"
  exit 0
fi

last_status_line=""
elapsed=0
while true; do
  runs_json="$("$GH_BIN" run list -R "$REPO" --commit "$SHA" --json databaseId,name,workflowName,status,conclusion,url,startedAt,updatedAt 2>/dev/null || echo '[]')"

  status_line="$(printf '%s' "$runs_json" | "$JQ_BIN" -r 'sort_by(.workflowName // .name) | map((.workflowName // .name) + ":" + .status + (if .conclusion then "(" + .conclusion + ")" else "" end)) | join(" ")')"
  if [ "$status_line" != "$last_status_line" ]; then
    echo "$(date '+%H:%M:%S') $status_line"
    last_status_line="$status_line"
  fi

  pending="$(printf '%s' "$runs_json" | "$JQ_BIN" -r '[.[] | select(.status != "completed")] | length')"
  if [ "$pending" -eq 0 ]; then
    break
  fi

  if [ "$elapsed" -ge "$TIMEOUT" ]; then
    echo ""
    echo "TIMEOUT: still running after ${TIMEOUT}s:"
    printf '%s' "$runs_json" | "$JQ_BIN" -r '.[] | select(.status != "completed") | (.workflowName // .name) + " " + .status + " " + .url'
    exit 3
  fi

  sleep "$POLL_INTERVAL"
  elapsed=$((elapsed + POLL_INTERVAL))
done

# Annotate duration labels for the final table.
enriched="[]"
count="$(printf '%s' "$runs_json" | "$JQ_BIN" 'length')"
i=0
while [ "$i" -lt "$count" ]; do
  row="$(printf '%s' "$runs_json" | "$JQ_BIN" ".[$i]")"
  start="$(printf '%s' "$row" | "$JQ_BIN" -r '.startedAt // "null"')"
  end="$(printf '%s' "$row" | "$JQ_BIN" -r '.updatedAt // "null"')"
  dur="$(duration_label "$start" "$end")"
  row="$(printf '%s' "$row" | "$JQ_BIN" --arg d "$dur" '. + {durationLabel: $d}')"
  enriched="$(printf '%s\n%s' "$enriched" "$row" | "$JQ_BIN" -s '.[0] + [.[1]]')"
  i=$((i + 1))
done

echo ""
print_table "$enriched"

if print_failures_and_exit "$enriched"; then
  echo ""
  echo "CI GREEN for $SHA"
  exit 0
else
  exit 1
fi

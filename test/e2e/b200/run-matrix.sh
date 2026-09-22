#!/usr/bin/env bash
# B200 validation matrix runner (#1376).
#
# Composes the existing tooling: each row applies a Model + InferenceService
# manifest, waits Ready, runs `llmkube benchmark -o json`, asserts, and hands
# the result to lib/capture.sh. Rows are individually runnable and idempotent
# because hardware access gets interrupted; the harness resumes, never
# restarts.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/lib/common.sh"
# shellcheck source=/dev/null
. "$here/lib/preflight.sh"
# shellcheck source=/dev/null
. "$here/lib/capture.sh"

usage() {
  cat <<'EOF'
Usage: run-matrix.sh --rows <list> [--out <dir>] [--namespace <ns>] [--dry-run] [--force]

  --rows 1,2,3,4   Matrix rows to run. Each row is independent.
  --out <dir>      Result directory (default: ./results).
  --namespace <ns> Namespace for the InferenceService (default: default).
  --dry-run        Validate harness wiring and fixtures; never deploys.
  --force          Re-run rows whose result file already exists.
EOF
}

rows_spec=""
out_dir=""
ns="default"
dry_run=0
force=0

while [ $# -gt 0 ]; do
  case "$1" in
    --rows)
      rows_spec="${2:-}"
      shift 2
      ;;
    --out)
      out_dir="${2:-}"
      shift 2
      ;;
    --namespace|-n)
      ns="${2:-}"
      shift 2
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    --force)
      force=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      b200_die "unknown argument: $1"
      ;;
  esac
done

[ -n "$rows_spec" ] || { usage >&2; b200_die "--rows is required"; }

rows_dir="$here/rows"
: "${out_dir:=./results}"
mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"

export B200_LIB="$here/lib"
export B200_REPO="$(b200_repo_root)"
export B200_OUT="$out_dir"
export B200_NS="$ns"
export B200_DRY_RUN="$dry_run"
export B200_MANIFESTS="$here/manifests"
export B200_FIXTURES="$here/fixtures"
export B200_DOC="$B200_REPO/docs/operations/b200-validation-matrix.md"

if [ "$dry_run" = "1" ]; then
  b200_log "dry-run: validating harness wiring against fixtures; no deploy attempted"
  pf_preflight_selfcheck || b200_die "preflight self-check failed; harness wiring is broken"
else
  b200_require_cmd kubectl llmkube
  pf_preflight_live || b200_die "preflight gate failed; refusing to run rows"
fi

processed=0
failed=0

for n in $(printf '%s' "$rows_spec" | tr ',' ' '); do
  case "$n" in
    ''|*[!0-9]*) b200_die "--rows accepts digits only, got: '$n'" ;;
  esac

  padded="$(printf '%02d' "$n")"
  script=""
  for f in "$rows_dir/$padded-"*.sh; do
    if [ -e "$f" ]; then
      script="$f"
      break
    fi
  done
  [ -n "$script" ] || b200_die "no row script for row $n ($rows_dir/$padded-*.sh)"

  result="$out_dir/$n.json"
  if [ -f "$result" ] && [ "$force" != "1" ]; then
    b200_log "skip row $n (already captured: $result); pass --force to re-run"
    continue
  fi

  b200_log "row $n: $script"
  # shellcheck source=/dev/null
  . "$script"
  processed=$((processed + 1))
  if ! b200_row_main; then
    failed=$((failed + 1))
  fi
done

b200_log "matrix complete: $processed row(s) run, $failed failed"
[ "$failed" -eq 0 ]

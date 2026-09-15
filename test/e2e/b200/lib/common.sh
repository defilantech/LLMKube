#!/usr/bin/env bash
# Shared helpers for the B200 validation harness (test/e2e/b200).
#
# Sourced by run-matrix.sh and the row scripts, never executed directly.

# b200_repo_root prints the repository root, derived from this file's location
# (test/e2e/b200/lib -> ../../../..).
b200_repo_root() {
  local here
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  (cd "$here/../../../.." && pwd)
}

b200_log() {
  printf '[b200] %s\n' "$*" >&2
}

b200_die() {
  b200_log "ERROR: $*"
  exit 1
}

# b200_require_cmd <cmd>...: fail early when a harness dependency is missing.
b200_require_cmd() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || b200_die "required command not found: $c"
  done
}

# b200_float_ge <a> <b>: true when a >= b. Avoids awk/bc for portability.
b200_float_ge() {
  local a="$1" b="$2"
  [ -n "$a" ] || return 1
  case "$a" in
    ''|*[!0-9.eE+-]*) return 1 ;;
  esac
  printf '%s\n%s\n' "$b" "$a" | sort -g | tail -n1 | grep -qx -- "$a"
}

# b200_standard_row drives the shared row shape for the serving rows:
# apply the manifest, wait Ready, benchmark to JSON, assert a decode floor,
# normalize the result. Rows 5-10 carry their own assertions instead.
#
# Args: <row> <inferenceservice-name> <manifest> <min-decode-tok-s>
b200_standard_row() {
  local row="$1" isvc="$2" manifest="$3" min_decode="$4"
  local out="$B200_OUT/$row"

  if [ "${B200_DRY_RUN:-0}" = "1" ]; then
    b200_log "row $row: dry-run: apply $manifest, wait Ready, benchmark, assert decode >= $min_decode tok/s"
    cap_normalize_dry_run "$out.json" "$row"
    return 0
  fi

  b200_require_cmd kubectl llmkube jq
  kubectl apply -f "$manifest"
  kubectl wait --for=jsonpath='{.status.phase}'=Ready "inferenceservice/$isvc" \
    -n "$B200_NS" --timeout=1800s
  llmkube benchmark "$isvc" -n "$B200_NS" -o json > "$out-benchmark.json"

  if [ "$(jq -r '.successful_runs' "$out-benchmark.json")" -lt 1 ]; then
    b200_log "row $row: FAIL: benchmark reported no successful runs"
    cap_normalize "$out-benchmark.json" "$out.json" "$row" fail
    cap_publish "$row" fail
    return 1
  fi

  local decode
  decode="$(jq -r '.generation_toks_per_sec_mean' "$out-benchmark.json")"
  if ! b200_float_ge "$decode" "$min_decode"; then
    b200_log "row $row: FAIL: decode $decode tok/s below floor $min_decode tok/s"
    cap_normalize "$out-benchmark.json" "$out.json" "$row" fail
    cap_publish "$row" fail
    return 1
  fi

  b200_log "row $row: PASS: decode $decode tok/s >= $min_decode tok/s"
  cap_normalize "$out-benchmark.json" "$out.json" "$row" pass
  cap_publish "$row" pass
}

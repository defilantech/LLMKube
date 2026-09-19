#!/usr/bin/env bash
# Result capture and matrix-doc publishing for the B200 harness.
#
# Every row outcome is normalized to JSON here, and cap_write_row is the only
# writer of the matrix doc's status cells, so the published status cannot drift
# from a captured run. Sourced after lib/common.sh.

# Path to the matrix doc. Defaults to the published doc in the repo.
: "${B200_DOC:=$(b200_repo_root)/docs/operations/b200-validation-matrix.md}"

# cap_normalize <benchmark.json> <out.json> <row> <status> normalizes an
# `llmkube benchmark -o json` result into the harness row schema.
cap_normalize() {
  local src="$1" out="$2" row="$3" status="$4" tmp
  [ -f "$src" ] || b200_die "cap_normalize: benchmark output not found: $src"
  b200_require_cmd jq

  tmp="$(mktemp)"
  jq --arg row "$row" --arg status "$status" \
    '{
       row: $row,
       status: $status,
       service_name: .service_name,
       iterations: .iterations,
       successful_runs: .successful_runs,
       generation_toks_per_sec_mean: .generation_toks_per_sec_mean,
       generation_toks_per_sec_min: .generation_toks_per_sec_min,
       generation_toks_per_sec_max: .generation_toks_per_sec_max,
       prompt_toks_per_sec_mean: .prompt_toks_per_sec_mean,
       latency_p50_ms: .latency_p50_ms,
       latency_p95_ms: .latency_p95_ms,
       timestamp: .timestamp
     }' "$src" > "$tmp"
  mv "$tmp" "$out"
}

# cap_normalize_dry_run <out.json> <row> writes a placeholder result so a
# dry-run exercises the harness wiring without claiming a row outcome.
cap_normalize_dry_run() {
  local out="$1" row="$2" tmp
  b200_require_cmd jq
  tmp="$(mktemp)"
  jq -n --arg row "$row" \
    '{
       row: $row,
       status: "dry-run",
       service_name: null,
       iterations: 0,
       successful_runs: 0,
       generation_toks_per_sec_mean: null,
       prompt_toks_per_sec_mean: null,
       latency_p50_ms: null,
       latency_p95_ms: null,
       timestamp: null
     }' > "$tmp"
  mv "$tmp" "$out"
}

# cap_write_row <doc> <row> <status> rewrites the status cell of one matrix
# row. status is one of pass / fail / blocked / inprogress / deferred.
cap_write_row() {
  local doc="$1" row="$2" status="$3"
  local label tmp replaced=0 line rest test notes

  case "$status" in
    pass) label="✅ pass" ;;
    fail) label="❌ fail" ;;
    blocked) label="⏳ blocked-by-hardware" ;;
    inprogress) label="🟡 in progress" ;;
    deferred) label="🚫 deferred" ;;
    *) b200_die "cap_write_row: unknown status '$status'" ;;
  esac

  [ -f "$doc" ] || b200_die "cap_write_row: matrix doc not found: $doc"

  tmp="$(mktemp)"
  while IFS= read -r line; do
    case "$line" in
      "| $row | "*)
        rest="${line#"| $row | "}"
        test="${rest%% | *}"
        notes="${rest#* | }"
        notes="${notes#* | }"
        printf '| %s | %s | %s | %s\n' "$row" "$test" "$label" "$notes" >> "$tmp"
        replaced=1
        ;;
      *)
        printf '%s\n' "$line" >> "$tmp"
        ;;
    esac
  done < "$doc"

  if [ "$replaced" -ne 1 ]; then
    rm -f "$tmp"
    b200_die "cap_write_row: row $row not found in $doc"
  fi
  mv "$tmp" "$doc"
}

# cap_publish <row> <status> flips the row's status in the matrix doc. Called
# only for a real pass/fail; a dry-run never touches the published doc.
cap_publish() {
  local row="$1" status="$2"
  cap_write_row "$B200_DOC" "$row" "$status"
  b200_log "published row $row as $status in $B200_DOC"
}

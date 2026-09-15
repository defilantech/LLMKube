#!/usr/bin/env bash
# Hardware-free self-test for the B200 validation harness.
#
# Exercises the preflight gate, the capture round-trip, the matrix-doc
# rewrite, and the dry-run resume guard against fixtures, so the harness wiring
# is verifiable in CI without a Blackwell node. A regression in any of these
# paths fails here rather than on hardware day.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
. "$here/lib/common.sh"
# shellcheck source=/dev/null
. "$here/lib/preflight.sh"
# shellcheck source=/dev/null
. "$here/lib/capture.sh"

export B200_FIXTURES="$here/fixtures"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp:-}"' EXIT

passes=0
failures=0

ok() {
  passes=$((passes + 1))
  b200_log "ok   - $1"
}

bad() {
  failures=$((failures + 1))
  b200_log "FAIL - $1"
}

assert_contains() {
  local hay="$1" needle="$2" name="$3"
  if printf '%s' "$hay" | grep -qF -- "$needle"; then
    ok "$name"
  else
    bad "$name (expected to contain: $needle)"
  fi
}

assert_eq() {
  local got="$1" want="$2" name="$3"
  if [ "$got" = "$want" ]; then
    ok "$name"
  else
    bad "$name (got: $got, want: $want)"
  fi
}

assert_ne_zero() {
  local got="$1" name="$2"
  if [ "$got" != "0" ]; then
    ok "$name"
  else
    bad "$name (expected non-zero, got 0)"
  fi
}

b200_require_cmd jq

# Preflight: a compliant node passes every floor and the Fabric Manager match.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-ok.kv" 2>&1)"
rc=$?
assert_eq "$rc" "0" "preflight accepts a compliant node"

# Preflight: a below-floor driver is rejected and named.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-below.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight rejects a below-floor node"
assert_contains "$out" "nvidiaDriver 570.30.01 below >= 580.173.02" "preflight names the driver shortfall"

# Preflight: a Fabric Manager that does not match the driver is rejected.
out="$(pf_assert_floors "$B200_FIXTURES/floors.yaml" "$B200_FIXTURES/measured-fm-mismatch.kv" 2>&1)"
rc=$?
assert_ne_zero "$rc" "preflight rejects a Fabric Manager mismatch"
assert_contains "$out" "fabricManager 580.150.00 does not match driver 580.173.02" "preflight names the Fabric Manager mismatch"

# Capture: a benchmark result round-trips into the harness row schema.
cap_normalize "$B200_FIXTURES/benchmark-sample.json" "$tmp/01.json" 1 pass
assert_eq "$(jq -r '.row' "$tmp/01.json")" "1" "capture records the row"
assert_eq "$(jq -r '.status' "$tmp/01.json")" "pass" "capture records the status"
assert_eq "$(jq -r '.generation_toks_per_sec_mean' "$tmp/01.json")" "87.5" "capture preserves decode throughput"

# Publish: the row's status cell flips, and no other row moves.
cp "$B200_DOC" "$tmp/matrix.md"
cap_write_row "$tmp/matrix.md" 1 pass
row1="$(grep -F '| 1 | ' "$tmp/matrix.md" | head -n1)"
row10="$(grep -F '| 10 | ' "$tmp/matrix.md" | head -n1)"
assert_contains "$row1" "✅ pass" "matrix row 1 flips to pass"
if printf '%s' "$row1" | grep -qF "blocked-by-hardware"; then
  bad "matrix row 1 is no longer blocked"
else
  ok "matrix row 1 is no longer blocked"
fi
assert_contains "$row10" "blocked-by-hardware" "unrelated rows are untouched"

# Resume: a captured row is skipped on re-run, so an interrupted session
# resumes instead of restarting.
run1="$( "$here/run-matrix.sh" --rows 1 --dry-run --out "$tmp/results" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "dry-run of row 1 succeeds"
assert_contains "$run1" "dry-run: validating harness wiring" "dry-run validates harness wiring"
run2="$( "$here/run-matrix.sh" --rows 1 --dry-run --out "$tmp/results" 2>&1 )"
rc=$?
assert_eq "$rc" "0" "re-run of a captured row succeeds"
assert_contains "$run2" "skip row 1 (already captured" "a captured row is skipped on re-run"

b200_log "self-test: $passes passed, $failures failed"
[ "$failures" -eq 0 ]

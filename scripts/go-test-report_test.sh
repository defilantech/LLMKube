#!/usr/bin/env bash
#
# Regression test for hack/go-test-report.sh summary, the CI "Test timing
# report" (#2141). It pins the two sections #2382 added: each budgeted
# package's time against its budget, with a ::warning:: annotation (never a
# failure) for one over it, and the top-level tests that grew most since a
# baseline report, which degrades to "no baseline" when CI has none to give.
#
# The fixtures in scripts/testdata/go-test-report are go test -json reports
# with make's plain lines interleaved, as CI records them. The over-budget one
# puts internal/controller at 612.503s against the script's real 600s budget,
# so raising that budget past the measured time fails this test.
#
# Usage: scripts/go-test-report_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REPORT="$REPO_ROOT/hack/go-test-report.sh"
FIXTURES="$SCRIPT_DIR/testdata/go-test-report"
[[ -f "$REPORT" ]] || { echo "FAIL - cannot find hack/go-test-report.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -40
  fails=$((fails+1))
}

# summary <args...>: sets CODE and OUT.
summary() {
  OUT="$(bash "$REPORT" summary "$@" 2>&1)" && CODE=0 || CODE=$?
}

# section <header>: the lines of OUT under "== <header>", up to the blank line
# that ends the section.
section() {
  awk -v h="== $1" '$0 == h { on = 1; next } on && $0 == "" { exit } on' <<<"$OUT"
}

M=github.com/defilantech/llmkube
# result <package> <test or ""> <action> <elapsed>: one go test -json result.
result() {
  if [[ -n "$2" ]]; then
    printf '{"Action":"%s","Package":"%s/%s","Test":"%s","Elapsed":%s}\n' "$3" "$M" "$1" "$2" "$4"
  else
    printf '{"Action":"%s","Package":"%s/%s","Elapsed":%s}\n' "$3" "$M" "$1" "$4"
  fi
}

# --- Budget ---------------------------------------------------------------

summary "$FIXTURES/report-over-budget.jsonl" 25
budget="$(section "Package time against budget")"
if [[ $CODE == 0 && "$budget" == *$'612.503s\t102% of 600s\tover budget\tinternal/controller'* \
  && "$OUT" == *$'\n::warning::internal/controller took 612.503s in report-over-budget.jsonl, over its 600s budget'* ]]; then
  pass "a package over its budget gets a ::warning:: annotation and exit 0"
else
  fail "a package over its budget gets a ::warning:: annotation and exit 0" "exit $CODE"
fi

summary "$FIXTURES/report-under-budget.jsonl" 25
budget="$(section "Package time against budget")"
if [[ $CODE == 0 && "$budget" == $'431.2s\t72% of 600s\twithin budget\tinternal/controller' && "$OUT" != *"::warning::"* ]]; then
  pass "a package within its budget is listed with no annotation"
else
  fail "a package within its budget is listed with no annotation" "exit $CODE"
fi

{ result internal/controller "" pass 600; } >"$WORK/at-budget.jsonl"
summary "$WORK/at-budget.jsonl" 25
if [[ $CODE == 0 && "$(section "Package time against budget")" == *"within budget"* && "$OUT" != *"::warning::"* ]]; then
  pass "a package exactly at its budget is within it"
else
  fail "a package exactly at its budget is within it" "exit $CODE"
fi

{ echo "build failed"; result pkg/cli "" pass 12.5; } >"$WORK/no-controller.jsonl"
summary "$WORK/no-controller.jsonl" 25
if [[ $CODE == 0 && "$(section "Package time against budget")" == $'no result\t600s budget\tinternal/controller' && "$OUT" != *"::warning::"* ]]; then
  pass "a budgeted package with no result says so without an annotation"
else
  fail "a budgeted package with no result says so without an annotation" "exit $CODE"
fi

# The two sections are additions: the ones #2141 prints keep their place.
summary "$FIXTURES/report-over-budget.jsonl" 25
headers="$(grep '^== ' <<<"$OUT")"
want="== Package time (slowest first)
== Package time against budget
== Failed tests
== Still running when the package exited (killed by -timeout or a panic)
== Slowest 25 top-level tests
== Top-level tests that grew most against the baseline"
if [[ "$headers" == "$want" ]]; then
  pass "the summary keeps its sections and adds budget and growth"
else
  fail "the summary keeps its sections and adds budget and growth" "headers: $headers"
fi

# --- Growth ---------------------------------------------------------------

# Against the baseline: two new tests, three that grew (one now failing), one
# that grew by a rounding-sized step, and none of: a test that shrank, one
# that was removed, one skipped now, and a subtest that grew by 12.86s.
summary "$FIXTURES/report-current.jsonl" 25 "$FIXTURES/report-baseline.jsonl"
want=$'+27.74s\tnew -> 27.74s\tinternal/controller\tTestMetalNodeSteadyMacStaysReachable
+16.03s\t71.22s -> 87.25s\tinternal/controller\tTestControllers
+12.74s\tnew -> 12.74s\tinternal/controller\tTestMultiFileCachedSize
+7s\t2s -> 9s\tpkg/cli\tTestMetalJoinStringIsSingleUse
+4.5s\t1s -> 5.5s\tinternal/controller\tTestFailsNow
+0.12s\t1.5s -> 1.62s\tinternal/controller\tTestSteady'
got="$(section "Top-level tests that grew most against the baseline")"
if [[ $CODE == 0 && "$got" == "$want" ]]; then
  pass "growth lists the grown and new top-level tests, largest first"
else
  fail "growth lists the grown and new top-level tests, largest first" "exit $CODE; got:
$got"
fi

# A report against itself: nothing grew.
summary "$FIXTURES/report-current.jsonl" 25 "$FIXTURES/report-current.jsonl"
if [[ $CODE == 0 && "$(section "Top-level tests that grew most against the baseline")" == "none grew" ]]; then
  pass "a report with no growth over its baseline says none grew"
else
  fail "a report with no growth over its baseline says none grew" "exit $CODE"
fi

# The list stops at 10 rows.
{
  result pkg/cli TestUnrelated pass 1
} >"$WORK/rows-baseline.jsonl"
{
  for i in 01 02 03 04 05 06 07 08 09 10 11 12; do result pkg/cli "TestNew$i" pass "$((100 - 10#$i))"; done
} >"$WORK/rows-current.jsonl"
summary "$WORK/rows-current.jsonl" 25 "$WORK/rows-baseline.jsonl"
got="$(section "Top-level tests that grew most against the baseline")"
if [[ $CODE == 0 && "$(wc -l <<<"$got" | tr -d ' ')" == 10 && "$got" == $'+99s\tnew'*TestNew01* && "$got" == *TestNew10 && "$got" != *TestNew11* ]]; then
  pass "growth lists at most 10 tests"
else
  fail "growth lists at most 10 tests" "exit $CODE; got:
$got"
fi

# No baseline is a line in the report, never a failed step: CI has none for
# a first run, an expired artifact, or a token that cannot read it.
nobase() {
  local name="$1" want="$2"; shift 2
  summary "$FIXTURES/report-current.jsonl" 25 "$@"
  if [[ $CODE == 0 && "$(section "Top-level tests that grew most against the baseline")" == "$want" ]]; then
    pass "$name"
  else
    fail "$name" "exit $CODE"
  fi
}
nobase "no baseline argument reports no baseline" "no baseline given"
nobase "a missing baseline file reports no baseline" \
  "no baseline: $WORK/absent/report.jsonl is missing or empty" "$WORK/absent/report.jsonl"
: >"$WORK/empty.jsonl"
nobase "an empty baseline file reports no baseline" \
  "no baseline: $WORK/empty.jsonl is missing or empty" "$WORK/empty.jsonl"
printf 'mkdir -p bin\n{"Action":"start","Package":"%s/pkg/cli"}\n{"Action":"pass","Pack' "$M" >"$WORK/truncated.jsonl"
nobase "a baseline with no test results reports no baseline" \
  "no baseline: $WORK/truncated.jsonl has no test results" "$WORK/truncated.jsonl"

# --- Stream ---------------------------------------------------------------

OUT="$(bash "$REPORT" stream <"$FIXTURES/report-current.jsonl" 2>&1)" && CODE=0 || CODE=$?
if [[ $CODE == 0 && "$OUT" == *$'ginkgo seed: 11 (pinned by GINKGO_SEED)\n'*"fails_test.go:12: boom"* ]]; then
  pass "stream still prints plain lines and test output"
else
  fail "stream still prints plain lines and test output" "exit $CODE"
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails go-test-report check(s) failed"
  exit 1
fi
echo "PASS: go-test-report summary budget and growth sections"

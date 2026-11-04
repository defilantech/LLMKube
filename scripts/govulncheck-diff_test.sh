#!/usr/bin/env bash
#
# Regression test for hack/govulncheck-diff.sh, which decides whether the
# govulncheck check fails a pull request or merge group (#2381): only on
# reachable findings the change introduces, while main pushes and the weekly
# scan (no base) fail on any reachable finding.
#
# The scans are built from scripts/testdata/govulncheck: each fixture is one
# vulnerability's messages as `govulncheck -format json` streams them (OSV
# entry, then module-, package- and symbol-level findings), and a head or base
# is the config fixture plus the vulnerabilities it has, concatenated.
#
# Usage: scripts/govulncheck-diff_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DIFF="$REPO_ROOT/hack/govulncheck-diff.sh"
FIX="$SCRIPT_DIR/testdata/govulncheck"
[[ -f "$DIFF" ]] || { echo "FAIL - cannot find hack/govulncheck-diff.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# scan <name> <fixture>...: concatenates fixtures into $WORK/<name>.json.
scan() {
  local name="$1" f
  shift
  for f in "$@"; do cat "$FIX/$f.json"; done >"$WORK/$name.json"
}

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -24
  fails=$((fails+1))
}

# compare <args>...: runs the script on $WORK files, sets CODE and OUT.
compare() {
  OUT="$(bash "$DIFF" "$@" 2>&1)" && CODE=0 || CODE=$?
}

# section <pattern>: the lines of OUT from the first line matching <pattern>
# up to the next section.
section() {
  awk -v pat="$1" 'found && /^(###|Not called|<!--)/ { exit } $0 ~ pat { found = 1 } found' <<<"$OUT"
}

# check <description> <want-exit> <condition>...: <condition> is a command
# that must succeed; the case passes when both hold.
check() {
  local desc="$1" want="$2"
  shift 2
  if [[ "$CODE" == "$want" ]] && "$@"; then
    pass "$desc"
  else
    fail "$desc" "exit $CODE, want $want"
  fi
}
has() { grep -qF -- "$2" <<<"$1"; }
lacks() { ! grep -qF -- "$2" <<<"$1"; }

# main has a reachable stdlib finding and a dependency it imports but never calls.
scan base config stdlib-called celgo-imported

scan head-new-dep config stdlib-called celgo-imported dep-called
compare --head "$WORK/head-new-dep.json" --base "$WORK/base.json"
introduced="$(section 'introduced by this change')"
already="$(section '^### Already reachable on the base')"
both_listed() {
  has "$introduced" "[GO-2099-0001]" && lacks "$introduced" "GO-2026-6607" &&
    has "$already" "[GO-2026-6607]" && lacks "$already" "GO-2099-0001"
}
check "a reachable finding only in the head fails, and the base's is listed apart" 1 both_listed
marker_is() { [[ "$(tail -n 1 <<<"$OUT")" == "<!-- govulncheck-reachable: GO-2026-6607@stdlib GO-2099-0001@example.com/vulnmod -->" ]]; }
check "the marker lists every reachable finding in the head" 1 marker_is
check "a | in an advisory summary does not break the table" 1 has "$OUT" 'Panic in Parse \| Decode'

compare --head "$WORK/base.json" --base "$WORK/base.json"
already="$(section '^### Already reachable on the base')"
listed_with_symbols() {
  has "$OUT" "No reachable vulnerability is introduced by this change." &&
    has "$already" "\`crypto/tls.Conn.Handshake\`, \`crypto/tls.Conn.Read\`"
}
check "a reachable finding in both passes and is listed with its symbols" 0 listed_with_symbols

scan head-bumped config stdlib-called celgo-imported
sed -i.bak 's/"v1.27.0"/"v1.27.1"/' "$WORK/head-bumped.json"
compare --head "$WORK/head-bumped.json" --base "$WORK/base.json"
check "a module moved to another affected version still passes" 0 has "$OUT" "### Already reachable on the base: 1"

scan clean config
compare --head "$WORK/clean.json" --base "$WORK/clean.json"
no_rows() { lacks "$OUT" "| [GO-" && has "$OUT" "No reachable vulnerability is introduced by this change."; }
check "no findings passes" 0 no_rows
compare --head "$WORK/clean.json"
check "no findings passes without a base" 0 has "$OUT" "No reachable vulnerabilities."

scan head-required config stdlib-called celgo-imported dep-required
compare --head "$WORK/head-required.json" --base "$WORK/base.json"
check "a new finding that is only module-level (required, not called) passes" 0 has "$OUT" "No reachable vulnerability is introduced by this change."
scan unreached config celgo-imported dep-required
compare --head "$WORK/unreached.json"
check "without a base, findings nothing calls pass and are counted" 0 has "$OUT" "Not called, so not failing: 2 vulnerabilities"

scan head-calls config stdlib-called celgo-imported celgo-called
compare --head "$WORK/head-calls.json" --base "$WORK/base.json"
check "new code calling a vulnerability the base only imports fails" 1 has "$(section 'introduced by this change')" "[GO-2026-6094]"

scan head-fixed config celgo-imported
compare --head "$WORK/head-fixed.json" --base "$WORK/base.json"
check "a fixed finding passes and is listed as no longer reachable" 0 has "$(section '^### No longer reachable')" "[GO-2026-6607]"

compare --head "$WORK/base.json"
check "without a base every reachable finding fails (main pushes, schedule)" 1 has "$OUT" "**1 reachable vulnerability**, each failing this check:"

compare --head "$WORK/base.json" --base-failed
check "a base that could not be scanned fails on every reachable finding and says so" 1 has "$OUT" "**The base could not be scanned** (the job log has the error)"

printf 'not json\n' >"$WORK/garbage.json"
compare --head "$WORK/clean.json" --base "$WORK/garbage.json"
check "a base that is not govulncheck output cannot fail a clean head" 0 has "$OUT" "its output is not govulncheck symbol-level JSON"
compare --head "$WORK/head-new-dep.json" --base "$WORK/garbage.json"
check "a base that is not govulncheck output fails on every reachable finding" 1 has "$OUT" "**2 reachable vulnerabilities**, each failing this check:"

: >"$WORK/empty.json"
compare --head "$WORK/empty.json" --base "$WORK/base.json"
check "an empty head is an error, not a pass" 2 has "$OUT" "is not govulncheck JSON output"
scan head-module config-module-level dep-called
compare --head "$WORK/head-module.json"
check "a head scanned below symbol level is an error" 2 has "$OUT" "reachability needs -scan symbol"
compare --head "$WORK/base.json" --base "$WORK/base.json" --base-failed
check "--base with --base-failed is a usage error" 2 has "$OUT" "usage:"

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all govulncheck-diff checks passed"

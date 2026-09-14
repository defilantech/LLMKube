#!/usr/bin/env bash
#
# Regression test for hack/scan-images.sh's failure aggregation.
#
# Self-contained: stubs `go`, `docker` and `trivy` on PATH so the build steps
# are no-ops and the scan outcome is scripted, then asserts the harness reports
# every image's findings in one run instead of aborting on the first.
#
# Usage: scripts/scan-images_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCAN="$SCRIPT_DIR/../hack/scan-images.sh"
[[ -f "$SCAN" ]] || { echo "cannot find hack/scan-images.sh"; exit 1; }

STUB="$(mktemp -d)"
LOG="$(mktemp)"
trap 'rm -rf "$STUB" "$LOG"' EXIT

cat >"$STUB/go" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$STUB/docker" <<'SH'
#!/usr/bin/env bash
exit 0
SH
# Records the scanned tag; fails only for the tag named in FAIL_ID.
cat >"$STUB/trivy" <<'SH'
#!/usr/bin/env bash
tag="${@: -1}"
echo "$tag" >>"$TRIVY_LOG"
if [[ -n "$FAIL_ID" ]] && [[ "$tag" == *"$FAIL_ID"* ]]; then
  exit 1
fi
exit 0
SH
chmod +x "$STUB/go" "$STUB/docker" "$STUB/trivy"

fails=0

# run <desc> <expected-exit> <expected-scans> <FAIL_ID>
run() {
  local desc="$1" want_exit="$2" want_scans="$3" fail_id="$4"
  : >"$LOG"
  local out code scanned
  out="$(PATH="$STUB:$PATH" TRIVY_LOG="$LOG" FAIL_ID="$fail_id" VERSION=test \
    bash "$SCAN" 2>&1)" && code=0 || code=$?
  scanned="$(wc -l <"$LOG" | tr -d ' ')"
  if [[ "$code" == "$want_exit" && "$scanned" == "$want_scans" ]]; then
    echo "ok   - $desc"
  else
    echo "FAIL - $desc (exit $code, want $want_exit; scanned $scanned, want $want_scans)"
    echo "$out" | sed 's/^/       /' | head -4
    fails=$((fails+1))
  fi
}

run "first image's findings do not hide the rest" 1 4 controller
run "a mid-list finding still scans the tail"     1 4 foreman-agent
run "an all-clean run passes"                     0 4 none

if ((fails)); then
  echo "FAILED: $fails case(s)"; exit 1
fi
echo "PASS: scan-images.sh reports every image"

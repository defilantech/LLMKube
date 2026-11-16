#!/usr/bin/env bash
#
# Regression test for hack/scan-images.sh's failure aggregation.
#
# Self-contained: stubs `go`, `docker`, `trivy` and `make` on PATH so the build
# steps are no-ops and the scan outcome is scripted, then asserts the harness
# reports every image's findings in one run instead of aborting on the first,
# and that each build context carries the license files the release
# Dockerfiles COPY.
#
# Usage: scripts/scan-images_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCAN="$SCRIPT_DIR/../hack/scan-images.sh"
[[ -f "$SCAN" ]] || { echo "cannot find hack/scan-images.sh"; exit 1; }

# The script runs the release's generated license tree when it is absent, so
# run it from a directory that holds LICENSE and no tree.
WORK="$(mktemp -d)"
STUB="$(mktemp -d)"
LOG="$(mktemp)"
STAGE_LOG="$(mktemp)"
trap 'rm -rf "$WORK" "$STUB" "$LOG" "$STAGE_LOG"' EXIT
touch "$WORK/LICENSE"

cat >"$STUB/go" <<'SH'
#!/usr/bin/env bash
exit 0
SH
# Records what the script stages into each build context instead of building.
cat >"$STUB/docker" <<'SH'
#!/usr/bin/env bash
ctx="${@: -1}"
find "$ctx" -type f | sed "s|^$ctx/||" >>"$STAGE_LOG"
exit 0
SH
# The script generates the license tree itself; produce the paths the release
# Dockerfiles COPY so the staged contexts can be checked.
cat >"$STUB/make" <<'SH'
#!/usr/bin/env bash
mkdir -p third_party_licenses/github.com/example/dep
: >third_party_licenses/github.com/example/dep/LICENSE
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
chmod +x "$STUB/go" "$STUB/docker" "$STUB/make" "$STUB/trivy"

fails=0

# run <desc> <expected-exit> <expected-scans> <FAIL_ID>
run() {
  local desc="$1" want_exit="$2" want_scans="$3" fail_id="$4"
  : >"$LOG"
  : >"$STAGE_LOG"
  local out code scanned
  out="$(cd "$WORK" && PATH="$STUB:$PATH" TRIVY_LOG="$LOG" STAGE_LOG="$STAGE_LOG" FAIL_ID="$fail_id" VERSION=test \
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

# stage <desc> <want_count>: the script stages LICENSE and the license tree
# into every context, as the release Dockerfiles COPY them (#2564).
stage() {
  local desc="$1" want="$2" have_license have_tree
  have_license="$(grep -c '^LICENSE$' "$STAGE_LOG" || true)"
  have_tree="$(grep -c '^third_party_licenses/github.com/example/dep/LICENSE$' "$STAGE_LOG" || true)"
  if [[ "$have_license" == "$want" && "$have_tree" == "$want" ]]; then
    echo "ok   - $desc: every context carries the license files"
  else
    echo "FAIL - $desc: every context carries LICENSE and the license tree (LICENSE in $have_license contexts, tree in $have_tree, want $want each)"
    fails=$((fails+1))
  fi
}

run "first image's findings do not hide the rest" 1 4 controller
stage "first image's findings do not hide the rest" 4
run "a mid-list finding still scans the tail"     1 4 foreman-agent
stage "a mid-list finding still scans the tail" 4
run "an all-clean run passes"                     0 4 none
stage "an all-clean run passes" 4

if ((fails)); then
  echo "FAILED: $fails case(s)"; exit 1
fi
echo "PASS: scan-images.sh reports every image"

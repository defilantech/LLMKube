#!/usr/bin/env bash
#
# Regression test for hack/govulncheck-tracking-issue.sh, which keeps one open
# issue in step with govulncheck on main (#2381): opened when main has
# reachable findings, its body replaced by each new report, a comment only when
# the set of findings changes, closed by the first clean scan, and nothing done
# for a rerun of the same report or for a report it cannot read.
#
# The reports are hack/govulncheck-diff.sh output for fixture scans from
# scripts/testdata/govulncheck, so the marker the issue script reads is the one
# the diff script writes. gh is a stub on PATH that keeps the issues in a JSON
# file and logs every call.
#
# Usage: scripts/govulncheck-tracking-issue_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TRACK="$REPO_ROOT/hack/govulncheck-tracking-issue.sh"
DIFF="$REPO_ROOT/hack/govulncheck-diff.sh"
FIX="$SCRIPT_DIR/testdata/govulncheck"
[[ -f "$TRACK" ]] || { echo "FAIL - cannot find hack/govulncheck-tracking-issue.sh"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
export GH_STUB_STATE="$WORK"

cat >"$WORK/bin/gh" <<'SH'
#!/usr/bin/env bash
# Stands in for the gh issue commands the tracking script runs.
set -euo pipefail
echo "$*" >>"$GH_STUB_STATE/calls.log"
db="$GH_STUB_STATE/issues.json"
[[ "$1" == issue ]] || { echo "gh stub: unexpected $*" >&2; exit 1; }
verb="$2"
shift 2
num="" title="" label="" body_file="" comment="" state=""
if [[ "$verb" != list && "$verb" != create ]]; then num="$1"; shift; fi
while [[ $# -gt 0 ]]; do
  case "$1" in
    --title) title="$2"; shift 2 ;;
    --label) label="$2"; shift 2 ;;
    --body-file) body_file="$2"; shift 2 ;;
    --comment) comment="$2"; shift 2 ;;
    --state) state="$2"; shift 2 ;;
    --limit|--json) shift 2 ;;
    *) echo "gh stub: unexpected flag $1" >&2; exit 1 ;;
  esac
done
update() { jq "$@" "$db" >"$db.tmp" && mv "$db.tmp" "$db"; }
case "$verb" in
  list)
    jq --arg state "$state" --arg label "$label" \
      '[.[] | select(.state == $state and any(.labels[]; . == $label)) | {number, title, body}]' "$db" ;;
  create)
    n=$(( $(jq '[.[].number] | max // 0' "$db") + 1 ))
    update --argjson n "$n" --arg t "$title" --arg l "$label" --rawfile b "$body_file" \
      '. + [{number: $n, title: $t, labels: [$l], state: "open", body: $b, comments: []}]'
    echo "https://github.com/stub/stub/issues/$n" ;;
  edit)
    update --argjson n "$num" --rawfile b "$body_file" 'map(if .number == $n then .body = $b else . end)' ;;
  comment)
    update --argjson n "$num" --rawfile b "$body_file" 'map(if .number == $n then .comments += [$b] else . end)' ;;
  close)
    update --argjson n "$num" --arg c "$comment" \
      'map(if .number == $n then .state = "closed" | .comments += [$c] else . end)' ;;
  *) echo "gh stub: unexpected verb $verb" >&2; exit 1 ;;
esac
SH
chmod +x "$WORK/bin/gh"

TITLE="[SECURITY] govulncheck findings on main"

# An unrelated open security issue and the tracking issue of an earlier,
# fixed incident: neither may be touched.
cat >"$WORK/issues.json" <<JSON
[
  {"number": 1, "title": "Harden the webhook TLS config", "labels": ["area/security"], "state": "open", "body": "unrelated", "comments": []},
  {"number": 2, "title": "$TITLE", "labels": ["area/security"], "state": "closed", "body": "an earlier incident", "comments": []}
]
JSON
: >"$WORK/calls.log"

# report <name> <fixture>...: hack/govulncheck-diff.sh's report, without a
# base, for a scan made of the fixtures.
report() {
  local name="$1" f
  shift
  for f in "$@"; do cat "$FIX/$f.json"; done >"$WORK/$name.json"
  bash "$DIFF" --head "$WORK/$name.json" >"$WORK/$name.md" || true
}
report one config stdlib-called celgo-imported
report two config stdlib-called celgo-imported dep-called
report clean config celgo-imported
printf '## govulncheck\n\nsomething went wrong\n' >"$WORK/nomarker.md"

fails=0
pass() { echo "ok   - $1"; }
fail() {
  echo "FAIL - $1: $2"
  [[ -n "${OUT:-}" ]] && echo "$OUT" | sed 's/^/       /' | head -12
  fails=$((fails+1))
}

# track <report> <run-url>: runs the script, sets CODE and OUT, and sets
# MUTATIONS to the gh calls it made other than listing.
track() {
  local before
  before="$(wc -l <"$WORK/calls.log")"
  OUT="$(PATH="$WORK/bin:$PATH" bash "$TRACK" "$WORK/$1.md" "$2" 2>&1)" && CODE=0 || CODE=$?
  MUTATIONS="$(tail -n +"$((before + 1))" "$WORK/calls.log" | grep -v '^issue list' || true)"
}
q() { jq -r "$@" "$WORK/issues.json"; }
# shellcheck disable=SC2016 # a jq program: $t is a jq variable
tracking_open() { q --arg t "$TITLE" '[.[] | select(.title == $t and .state == "open")] | length'; }
untouched_others() {
  [[ "$(q '.[] | select(.number == 1) | "\(.state) \(.body) \(.comments | length)"')" == "open unrelated 0" &&
     "$(q '.[] | select(.number == 2) | "\(.state) \(.body) \(.comments | length)"')" == "closed an earlier incident 0" ]]
}

track one https://example.test/runs/1
if [[ $CODE == 0 && "$(tracking_open)" == 1 ]] &&
   [[ "$(q '.[] | select(.number == 3) | .labels | join(",")')" == area/security ]] &&
   q '.[] | select(.number == 3) | .body' | grep -qF 'Latest scan: https://example.test/runs/1' &&
   q '.[] | select(.number == 3) | .body' | grep -qF '[GO-2026-6607]' &&
   [[ "$(q '.[] | select(.number == 3) | .comments | length')" == 0 ]] && untouched_others; then
  pass "reachable findings open one labeled issue with the report, leaving other issues alone"
else
  fail "reachable findings open one labeled issue with the report, leaving other issues alone" "exit $CODE"
fi

track one https://example.test/runs/1
if [[ $CODE == 0 && -z "$MUTATIONS" && "$(tracking_open)" == 1 ]]; then
  pass "a rerun with the same report changes nothing"
else
  fail "a rerun with the same report changes nothing" "exit $CODE, calls: $MUTATIONS"
fi

track one https://example.test/runs/2
if [[ $CODE == 0 && "$MUTATIONS" == "issue edit 3 --body-file "* ]] &&
   q '.[] | select(.number == 3) | .body' | grep -qF 'Latest scan: https://example.test/runs/2' &&
   [[ "$(q '.[] | select(.number == 3) | .comments | length')" == 0 ]]; then
  pass "a later scan with the same findings updates the body without a comment"
else
  fail "a later scan with the same findings updates the body without a comment" "exit $CODE, calls: $MUTATIONS"
fi

track two https://example.test/runs/3
comment="$(q '.[] | select(.number == 3) | .comments[-1] // ""')"
if [[ $CODE == 0 && "$(tracking_open)" == 1 ]] &&
   q '.[] | select(.number == 3) | .body' | grep -qF '[GO-2099-0001]' &&
   grep -qF -- '- Now reachable: GO-2099-0001@example.com/vulnmod' <<<"$comment" &&
   ! grep -qF 'No longer reachable' <<<"$comment"; then
  pass "a new finding updates the issue and comments with what was added"
else
  fail "a new finding updates the issue and comments with what was added" "exit $CODE, comment: $comment"
fi

track one https://example.test/runs/4
comment="$(q '.[] | select(.number == 3) | .comments[-1] // ""')"
if [[ $CODE == 0 && "$(q '.[] | select(.number == 3) | .comments | length')" == 2 ]] &&
   grep -qF -- '- No longer reachable: GO-2099-0001@example.com/vulnmod' <<<"$comment"; then
  pass "a fixed finding comments with what was removed"
else
  fail "a fixed finding comments with what was removed" "exit $CODE, comment: $comment"
fi

track nomarker https://example.test/runs/5
if [[ $CODE == 2 && -z "$MUTATIONS" && "$(tracking_open)" == 1 ]]; then
  pass "a report without the marker changes nothing and fails"
else
  fail "a report without the marker changes nothing and fails" "exit $CODE, calls: $MUTATIONS"
fi

track clean https://example.test/runs/6
if [[ $CODE == 0 && "$(tracking_open)" == 0 ]] &&
   q '.[] | select(.number == 3) | .comments[-1]' | grep -qF 'https://example.test/runs/6' && untouched_others; then
  pass "a clean scan closes the issue with a comment naming the run"
else
  fail "a clean scan closes the issue with a comment naming the run" "exit $CODE"
fi

track clean https://example.test/runs/7
if [[ $CODE == 0 && -z "$MUTATIONS" ]]; then
  pass "a clean scan with no open issue changes nothing"
else
  fail "a clean scan with no open issue changes nothing" "exit $CODE, calls: $MUTATIONS"
fi

track one https://example.test/runs/8
if [[ $CODE == 0 && "$(tracking_open)" == 1 && "$(q '.[] | select(.state == "open" and .title != "Harden the webhook TLS config") | .number')" == 4 ]]; then
  pass "findings after a close open a new issue instead of reopening the old one"
else
  fail "findings after a close open a new issue instead of reopening the old one" "exit $CODE"
fi

if [[ $fails -gt 0 ]]; then
  echo "$fails failure(s)"
  exit 1
fi
echo "all govulncheck-tracking-issue checks passed"

#!/usr/bin/env bash
#
# Keeps one open GitHub issue in step with govulncheck on main (#2381).
#
# Pull requests fail only on the vulnerabilities they introduce
# (hack/govulncheck-diff.sh), so a new advisory against code already on main
# shows up in the main pushes and the weekly scan. The Security workflow calls
# this from those runs with their report: while main has reachable findings,
# one open issue carries the latest report as its body and gets a comment
# whenever the set of findings changes; once a scan of main is clean, the issue
# is closed. Rerunning with the same report changes nothing.
#
#   hack/govulncheck-tracking-issue.sh REPORT.md RUN_URL
#
# REPORT.md is hack/govulncheck-diff.sh output for a scan without a base; its
# hidden govulncheck-reachable marker says what is reachable, and a report
# without the marker changes nothing. The issue is the oldest open one with
# the area/security label and the exact title below, so it is found by a
# listing, not the search index, which can lag a just-created issue. Needs gh
# with GH_TOKEN (issues: write) and GH_REPO.
set -euo pipefail
# sort and comm must collate alike.
export LC_ALL=C

TITLE="[SECURITY] govulncheck findings on main"
LABEL="area/security"
MARKER_RE='^<!-- govulncheck-reachable:.* -->$'

if [[ $# -ne 2 ]]; then
  echo "usage: $0 REPORT.md RUN_URL" >&2
  exit 2
fi
report="$1" run_url="$2"

if ! grep -qE "$MARKER_RE" "$report"; then
  echo "$report has no govulncheck-reachable marker; leaving the tracking issue alone" >&2
  exit 2
fi

# keys: the reachable findings a report or issue body on stdin marks, one per
# line and sorted; nothing when it marks none or has no marker.
keys() {
  sed -n 's/^<!-- govulncheck-reachable:\(.*\) -->$/\1/p' | tail -n 1 | tr ' ' '\n' | sed '/^$/d' | sort
}

new_keys="$(keys <"$report")"
issue="$(gh issue list --state open --label "$LABEL" --limit 200 --json number,title,body |
  jq -c --arg title "$TITLE" '[.[] | select(.title == $title)] | sort_by(.number) | first // empty')"
number="" old_body=""
if [[ -n "$issue" ]]; then
  number="$(jq -r .number <<<"$issue")"
  old_body="$(jq -r .body <<<"$issue")"
fi

if [[ -z "$new_keys" ]]; then
  if [[ -n "$number" ]]; then
    gh issue close "$number" --comment "govulncheck reports no reachable vulnerabilities on main as of [this run](${run_url}). Closing."
    echo "main is clean; closed #${number}"
  else
    echo "main is clean and no tracking issue is open"
  fi
  exit 0
fi

body="$(mktemp)"
note="$(mktemp)"
trap 'rm -f "$body" "$note"' EXIT
{
  echo "govulncheck finds reachable vulnerabilities on \`main\`. Pull requests that add none still pass (#2381), so this issue tracks them until they are fixed. Every main push and the weekly scan update it, and the first clean scan closes it."
  echo
  echo "Latest scan: ${run_url}"
  echo
  cat "$report"
} >"$body"

if [[ -z "$number" ]]; then
  gh issue create --title "$TITLE" --label "$LABEL" --body-file "$body"
  exit 0
fi

if [[ "$(cat "$body")" != "$old_body" ]]; then
  gh issue edit "$number" --body-file "$body"
fi

old_keys="$(keys <<<"$old_body")"
if [[ "$new_keys" != "$old_keys" ]]; then
  added="$(comm -13 <(echo "$old_keys") <(echo "$new_keys") | sed '/^$/d' | paste -sd ' ' -)"
  removed="$(comm -23 <(echo "$old_keys") <(echo "$new_keys") | sed '/^$/d' | paste -sd ' ' -)"
  {
    echo "The reachable findings on main changed in [this run](${run_url})."
    [[ -z "$added" ]] || echo "- Now reachable: ${added}"
    [[ -z "$removed" ]] || echo "- No longer reachable: ${removed}"
  } >"$note"
  gh issue comment "$number" --body-file "$note"
fi
echo "updated #${number}"

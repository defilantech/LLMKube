#!/usr/bin/env bash
#
# Compares two govulncheck JSON scans and fails only on the reachable findings
# the head has and the base does not (#2381).
#
# In text mode govulncheck fails on any reachable finding, so a new advisory
# against the Go standard library or a dependency already on main failed every
# open pull request until main was fixed: GO-2026-6599 to GO-2026-6617 on
# 2026-10-11 and GO-2026-6809 on 2026-10-21 failed 15 runs that changed
# nothing related. Pull requests and merge groups now scan the change and the
# commit it applies to, and fail only on what the change brings in.
#
#   hack/govulncheck-diff.sh --head HEAD.json [--base BASE.json | --base-failed]
#
# Both files are `govulncheck -format json ./...` output. Without a base, every
# reachable finding in the head fails (main pushes and the weekly scan).
# --base-failed says a base was wanted but could not be scanned: every
# reachable finding fails then too, and the report says why. A BASE.json that
# is not govulncheck symbol-level output counts as --base-failed, so a broken
# base scan cannot fail the check by itself.
#
# Reachable means govulncheck found a call path to the vulnerable symbol, which
# is what text mode fails on at the default symbol scan level (isCalled in
# golang.org/x/vuln/internal/scan/template.go). In the JSON stream
# (golang.org/x/vuln/internal/govulncheck, protocol v1.0.0) the first frame of
# such a finding's trace is the vulnerable symbol and names its function;
# module- and package-level findings have no function there. Findings are
# compared by OSV ID and module, so moving a module from one affected version
# to another, or adding a second call path to a symbol the base already
# reaches, does not fail; making a vulnerability the base only imports
# reachable does.
#
# Writes a Markdown report on stdout. Its last line is a hidden marker listing
# the head's reachable findings, which hack/govulncheck-tracking-issue.sh reads.
# Exit status: 0 when nothing new is reachable, 1 when something is, 2 on bad
# usage or a head file that is not govulncheck symbol-level output.
set -euo pipefail

usage() {
  echo "usage: $0 --head HEAD.json [--base BASE.json | --base-failed]" >&2
  exit 2
}

head_file="" base_file="" base_failed=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --head) [[ $# -ge 2 ]] || usage; head_file="$2"; shift 2 ;;
    --base) [[ $# -ge 2 ]] || usage; base_file="$2"; shift 2 ;;
    --base-failed) base_failed=true; shift ;;
    *) usage ;;
  esac
done
[[ -n "$head_file" ]] || usage
[[ -n "$base_file" && "$base_failed" == true ]] && usage

# scan_level <file> prints the scan level a govulncheck JSON stream declares in
# its config message, or nothing when the file is not such a stream.
scan_level() {
  jq -n -r '[inputs | .config? // empty | .scan_level // ""] | first // empty' "$1" 2>/dev/null || true
}

level="$(scan_level "$head_file")"
if [[ "$level" != symbol ]]; then
  if [[ -z "$level" ]]; then
    echo "$head_file is not govulncheck JSON output" >&2
  else
    echo "$head_file was scanned at $level level; reachability needs -scan symbol, the default" >&2
  fi
  exit 2
fi

base_note=""
if $base_failed; then
  base_note="the job log has the error"
elif [[ -n "$base_file" && "$(scan_level "$base_file")" != symbol ]]; then
  base_note="its output is not govulncheck symbol-level JSON"
  base_file=""
fi
have_base=false
[[ -n "$base_file" ]] && have_base=true

result="$(jq -n \
  --slurpfile head "$head_file" \
  --slurpfile base "${base_file:-/dev/null}" \
  --argjson have_base "$have_base" '
  def reachable: [.[] | .finding? // empty | select((.trace[0].function // "") != "")];
  def symbol: .trace[0]
    | "\(.package).\(if (.receiver // "") != "" then "\(.receiver | ltrimstr("*"))." else "" end)\(.function)";
  def key: "\(.osv)@\(.module)";
  # One row per OSV ID and module, however many call paths reach it.
  def rows($summary):
    group_by([.osv, .trace[0].module])
    | map({
        osv: .[0].osv,
        module: .[0].trace[0].module,
        version: ([.[].trace[0].version // empty] | first // ""),
        fixed: ([.[].fixed_version // empty] | first // ""),
        symbols: (map(symbol) | unique),
        summary: ($summary[.[0].osv] // "")
      });
  def set: map({key: key, value: true}) | from_entries;

  ([($head + $base)[] | .osv? // empty | {key: .id, value: (.summary // "")}] | from_entries) as $summary
  | ($head | reachable | rows($summary)) as $h
  | ($base | reachable | rows($summary)) as $b
  | ($b | set) as $in_base
  | ($h | set) as $in_head
  | {
      have_base: $have_base,
      new: (if $have_base then [$h[] | select($in_base[key] | not)] else $h end),
      existing: (if $have_base then [$h[] | select($in_base[key])] else [] end),
      gone: [$b[] | select($in_head[key] | not)],
      unreached: (([$head[] | .finding? // empty | .osv] | unique) - [$h[].osv] | length),
      reachable: ($h | map(key) | sort)
    }')"

jq -r --arg base_note "$base_note" '
  def plural($n; $one; $many): "\($n) \(if $n == 1 then $one else $many end)";
  def cell: tostring | gsub("[\r\n]+"; " ") | gsub("\\|"; "\\|");
  def symbols: if length > 4 then (.[:4] + ["and \(length - 4) more"]) else . end
    | map(if startswith("and ") then . else "`\(.)`" end) | join(", ");
  def table:
    "",
    "| OSV | Module | Fixed in | Vulnerable symbols called | Summary |",
    "| --- | --- | --- | --- | --- |",
    (.[] | "| [\(.osv)](https://pkg.go.dev/vuln/\(.osv)) | `\(.module)` \(.version) | \(if .fixed == "" then "no fix yet" else .fixed end) | \(.symbols | symbols) | \(.summary | cell) |");

  "## govulncheck",
  "",
  (if $base_note != "" then
     "> **The base could not be scanned** (\($base_note)), so this run cannot tell which findings the change introduced, and every reachable finding fails it.", ""
   else empty end),
  (if .have_base then
     (if (.new | length) == 0 then "No reachable vulnerability is introduced by this change."
      else "**\(plural(.new | length; "reachable vulnerability"; "reachable vulnerabilities")) introduced by this change**, failing this check:", (.new | table) end),
     (if (.existing | length) > 0 then
        "", "### Already reachable on the base: \(.existing | length), not failing this check",
        "These fail the scans of `main`, which keep a tracking issue open until they are fixed.",
        (.existing | table)
      else empty end),
     (if (.gone | length) > 0 then
        "", "### No longer reachable after this change: \(.gone | length)", (.gone | table)
      else empty end)
   else
     (if (.new | length) == 0 then "No reachable vulnerabilities."
      else "**\(plural(.new | length; "reachable vulnerability"; "reachable vulnerabilities"))**, each failing this check:", (.new | table) end)
   end),
  (if .unreached > 0 then
     "", "Not called, so not failing: \(plural(.unreached; "vulnerability"; "vulnerabilities")) in modules this code requires or packages it imports. `govulncheck -show verbose ./...` lists them."
   else empty end),
  "",
  "<!-- govulncheck-reachable:\(.reachable | map(" " + .) | join("")) -->"
' <<<"$result"

[[ "$(jq '.new | length' <<<"$result")" -eq 0 ]] || exit 1

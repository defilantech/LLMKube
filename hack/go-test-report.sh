#!/usr/bin/env bash
#
# go-test-report: read `go test -json` output, which make interleaves with its
# own plain lines (the echoed recipe, the ginkgo seed, build errors), and
# either stream it as ordinary text or summarize where the time went (#2141).
#
#   make test GO_TEST_FLAGS=-json | tee report.jsonl | hack/go-test-report.sh stream
#   hack/go-test-report.sh summary report.jsonl [N] [BASELINE]
#
# stream   prints every test's output text and passes non-JSON lines through,
#          so the CI log reads like `go test -v`.
# summary  prints each package's time, each budgeted package's time against
#          its budget (BUDGETS below), the failed tests, any test still
#          running when its package died (a -timeout kill leaves no result
#          line for it), the N slowest top-level tests (default 25), and the
#          top-level tests that grew most since BASELINE, an earlier report of
#          the same suite (CI passes the last green main run's). Without a
#          usable BASELINE that section says "no baseline".
#
# Needs bash and jq. Exits 0 in both modes: the go test exit status is the
# caller's to keep (CI runs the pipeline under bash -o pipefail). A package
# over its budget is a ::warning:: annotation, not a failure.
# scripts/go-test-report_test.sh tests the summary; run make test-scripts
# after editing this file.
set -euo pipefail

# Package time budgets in seconds, one "package seconds" pair per line (#2382).
# go test's -timeout (GO_TEST_TIMEOUT in the Makefile) stays the hard limit; a
# budget is the early warning below it. Leave headroom over the measured time:
# runner speed varies by a few percent from run to run.
BUDGETS='
internal/controller 600
'

# How many grown tests the summary lists.
GROWTH_ROWS=10

# One JSON event per line; anything that does not parse as a go test event is
# a plain line.
# shellcheck disable=SC2016 # a jq program: $line and $e are jq variables
EVENTS='. as $line | (try fromjson catch null) as $e
  | if ($e | type) == "object" and ($e | has("Action")) then $e else {plain: $line} end'

case "${1:-}" in
stream)
	jq -Rrj --unbuffered "$EVENTS"' | if has("plain") then .plain + "\n"
		elif .Action == "output" then .Output else empty end'
	;;
summary)
	file="${2:?usage: go-test-report.sh summary FILE [N] [BASELINE]}"
	n="${3:-25}"
	baseline="${4:-}"
	budgets="$(jq -Rn '[inputs | select(test("\\S"))
		| (capture("^\\s*(?<pkg>\\S+)\\s+(?<secs>[0-9]+(\\.[0-9]+)?)\\s*$")
			// error("go-test-report.sh: bad BUDGETS line: \(.)"))
		| .secs |= tonumber]' <<<"$BUDGETS")"
	if [[ -z "$baseline" ]]; then
		nobase="no baseline given"
	elif [[ ! -s "$baseline" ]]; then
		nobase="no baseline: $baseline is missing or empty"
	else
		nobase="no baseline: $baseline has no test results"
	fi
	# The baseline's top-level test results; the rest of a report is output
	# text the growth section does not read.
	base_results() {
		[[ -n "$baseline" && -s "$baseline" ]] || return 0
		jq -c -R "$EVENTS"' | select(.Test != null and (.Test | contains("/") | not)
			and (.Action == "pass" or .Action == "fail"))' "$baseline"
	}
	jq -R "$EVENTS"' | select(has("Action"))' "$file" | jq -rs --argjson n "$n" \
		--argjson rows "$GROWTH_ROWS" --argjson budgets "$budgets" \
		--arg report "$(basename "$file")" --arg nobase "$nobase" \
		--slurpfile base <(base_results) '
		def pkg: .Package | sub("^github.com/defilantech/llmkube/"; "");
		def secs: (.Elapsed // 0 | tostring) + "s";
		def results: map(select(.Action == "pass" or .Action == "fail" or .Action == "skip"));
		def r2: . * 100 | round / 100;
		def toplevel: reduce (.[] | select((.Action == "pass" or .Action == "fail")
				and (.Test | contains("/") | not))) as $e
			({}; .[$e.Package + "\u0000" + $e.Test] |= {pkg: ($e | pkg), test: $e.Test,
				secs: ((.secs // 0) + ($e.Elapsed // 0))});
		(map(select(.Test == null)) | results | sort_by(-(.Elapsed // 0))) as $pkgs
		| (map(select(.Test != null)) | results) as $done
		| ($done | map(.Package + "\u0000" + .Test) | INDEX(.)) as $finished
		| (map(select(.Test != null and .Action == "run"))
			| map(select($finished[.Package + "\u0000" + .Test] == null))) as $running
		| "== Package time (slowest first)",
		  ($pkgs[:15][] | "\(secs)\t\(.Action)\t\(pkg)"),
		  "",
		  "== Package time against budget",
		  ($budgets[] as $b
			| ([$pkgs[] | select(pkg == $b.pkg) | .Elapsed // 0] | max) as $t
			| if $t == null then "no result\t\($b.secs)s budget\t\($b.pkg)"
			  else "\($t)s\t\($t / $b.secs * 100 | round)% of \($b.secs)s\t\(if $t > $b.secs then "over budget" else "within budget" end)\t\($b.pkg)",
				(select($t > $b.secs)
					| "::warning::\($b.pkg) took \($t)s in \($report), over its \($b.secs)s budget (BUDGETS in hack/go-test-report.sh)")
			  end),
		  "",
		  "== Failed tests",
		  (($done | map(select(.Action == "fail"))) as $f
			| if ($f | length) == 0 then "none" else ($f[] | "\(secs)\t\(pkg)\t\(.Test)") end),
		  "",
		  "== Still running when the package exited (killed by -timeout or a panic)",
		  (if ($running | length) == 0 then "none" else ($running[] | "\(pkg)\t\(.Test)") end),
		  "",
		  "== Slowest \($n) top-level tests",
		  ($done | map(select(.Action != "skip" and (.Test | contains("/") | not)))
			| sort_by(-(.Elapsed // 0))[:$n][] | "\(secs)\t\(.Action)\t\(pkg)\t\(.Test)"),
		  "",
		  "== Top-level tests that grew most against the baseline",
		  (if ($base | length) == 0 then $nobase
		   else ($base | toplevel) as $was
			| [$done | toplevel | to_entries[] | .value + {was: $was[.key].secs}
				| .grew = .secs - (.was // 0) | select(.grew > 0)]
			| sort_by(-.grew)[:$rows]
			| if length == 0 then "none grew"
			  else .[] | "+\(.grew | r2)s\t\(if .was == null then "new" else "\(.was | r2)s" end) -> \(.secs | r2)s\t\(.pkg)\t\(.test)"
			  end
		   end)'
	;;
*)
	awk 'NR > 1 && !/^#/ { exit } NR > 1' "$0" >&2
	exit 2
	;;
esac

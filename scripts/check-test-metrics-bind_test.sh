#!/usr/bin/env bash
#
# Regression test for test managers binding the default metrics port (#2377).
#
# A controller-runtime manager built with default ctrl.Options serves metrics
# on :8080. go test runs packages in parallel, so two test packages that each
# start such a manager race for the port, and the loser's manager exits with
# "failed to start metrics server: ... bind: address already in use" before it
# reconciles anything. TestMetalNodeLeaseEventReachesItsMetalNode failed that
# way 8 times in two days on CI, on main and on unrelated PRs.
#
# The rule this enforces: every ctrl.NewManager or manager.New call in a
# _test.go file passes Metrics: metricsserver.Options{BindAddress: "0"}, which
# turns the listener off. Production managers (cmd/) keep theirs and are not
# checked.
#
# Usage: scripts/check-test-metrics-bind_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# unbound_managers FILE...: prints file:line for each manager constructor call
# whose argument list (up to its closing parenthesis) has no BindAddress: "0".
unbound_managers() {
	awk '
	FNR == 1 { inside = 0 }
	!inside && match($0, /(ctrl\.NewManager|manager\.New)\(/) {
		inside = 1; depth = 0; text = ""; start = FNR
		line = substr($0, RSTART + RLENGTH - 1)
	}
	inside {
		if (FNR != start) line = $0
		for (i = 1; i <= length(line); i++) {
			c = substr(line, i, 1)
			text = text c
			if (c == "(") depth++
			else if (c == ")") { depth--; if (depth == 0) break }
		}
		text = text "\n"
		if (depth == 0) {
			if (text !~ /BindAddress:[ \t]*"0"/) print FILENAME ":" start
			inside = 0
		}
	}
	' "$@"
}

# Prove the check bites before trusting a clean result: a default-options
# manager, a one-line one and a multi-line one with the listener off.
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
cat >"$fixture/fixture_test.go" <<'EOF'
func a() { mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme}) }
func b() {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
}
func c() { mgr, err := manager.New(cfg, manager.Options{Cache: cache.Options{}}) }
EOF
got="$(unbound_managers "$fixture/fixture_test.go" | sed "s#^$fixture/##" | tr '\n' ' ')"
if [[ "$got" != "fixture_test.go:1 fixture_test.go:8 " ]]; then
	echo "FAIL: the metrics-bind check misreads its fixture: got '$got'" >&2
	exit 1
fi

files=()
while IFS= read -r f; do
	[[ -n "$f" ]] && files+=("$ROOT/$f")
done < <(cd "$ROOT" && git ls-files --cached --others --exclude-standard -- '*_test.go')
if [[ ${#files[@]} -eq 0 ]]; then
	echo "FAIL: found no _test.go files under $ROOT; is git available?" >&2
	exit 1
fi

offenders="$(unbound_managers "${files[@]}" | sed "s#^$ROOT/##")"
if [[ -n "$offenders" ]]; then
	echo "FAIL: these test managers serve metrics on the default :8080:" >&2
	printf '%s\n' "$offenders" >&2
	echo "Pass Metrics: metricsserver.Options{BindAddress: \"0\"} in their ctrl.Options;" >&2
	echo "parallel test packages otherwise collide on the port (#2377)." >&2
	exit 1
fi

echo "PASS: no test manager listens on the default metrics port"

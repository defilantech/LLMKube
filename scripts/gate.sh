#!/usr/bin/env bash
#
# The agent gate: one command per round. It regenerates manifests and DeepCopy
# and fails if that changed files the working tree did not already have
# modified (uncommitted regeneration drift), checks gofmt without rewriting,
# checks that every Go file the branch adds carries the copyright year
# hack/boilerplate.go.txt does, runs go vet, and only then runs lint-all and
# the full test suite in parallel: nothing after vet regenerates, so lint and
# tests cannot race the generators. Every step logs to a file under GATE_DIR
# and the gate exits nonzero if any step failed, so a failing lint is not
# masked by passing tests or the reverse.
#
# The suites run with a pinned GINKGO_SEED (default 4242) instead of make
# test's per-run seed: go test caches a result only while the environment
# variables the test reads stay unchanged, and the envtest suites read
# GINKGO_SEED, so a fresh seed per run never reuses the cache. Override the
# seed from the environment: GINKGO_SEED=<n> make gate.
#
# Environment (all optional):
#   GATE_DIR          log directory (default ${TMPDIR:-/tmp}/llmkube-gate)
#   GATE_ROOT         repo root (default: the checkout this script lives in)
#   GINKGO_SEED       ginkgo seed for the test step (default 4242)
#   GO_TEST_TIMEOUT   go test -timeout for the test step (default 20m; go's
#                     own 10m default is too close for internal/controller,
#                     #2141)
#   LINT_GOTOOLCHAIN  GOTOOLCHAIN for the lint step only, e.g. go1.27.4
#                     (empty: use the default toolchain)
#   ENVTEST_BIN, BIN_DIR, ENVTEST_K8S_VERSION
#                     setup-envtest binary, local bin dir and k8s version;
#                     the Makefile passes these, standalone runs derive them
#   GATE_REGEN_CMD, GATE_FMT_CMD, GATE_COPYRIGHT_CMD, GATE_VET_CMD,
#   GATE_LINT_CMD, GATE_TEST_CMD
#                     step command overrides; scripts/gate_test.sh drives the
#                     gate with stubs through them
#
# Written for the bash that ships with macOS (no associative arrays).
set -euo pipefail

ROOT="${GATE_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "$ROOT"

GATE_DIR="${GATE_DIR:-${TMPDIR:-/tmp}/llmkube-gate}"
mkdir -p "$GATE_DIR"

: "${GINKGO_SEED:=4242}"
export GINKGO_SEED

LINT_GOTOOLCHAIN="${LINT_GOTOOLCHAIN:-}"

GATE_REGEN_CMD="${GATE_REGEN_CMD:-make manifests generate}"
GATE_FMT_CMD="${GATE_FMT_CMD:-}"
GATE_COPYRIGHT_CMD="${GATE_COPYRIGHT_CMD:-}"
GATE_VET_CMD="${GATE_VET_CMD:-go vet ./...}"
GATE_LINT_CMD="${GATE_LINT_CMD:-}"
GATE_TEST_CMD="${GATE_TEST_CMD:-}"

echo "gate: seed=$GINKGO_SEED dir=$GATE_DIR root=$ROOT${LINT_GOTOOLCHAIN:+ lint-gotoolchain=$LINT_GOTOOLCHAIN}"

# gofmt -l over tracked and untracked .go files, without rewriting. Paths git
# still lists but the working tree no longer has (a staged deletion) are
# skipped so they cannot fail the check.
fmt_check() {
	local files=() out f
	while IFS= read -r -d '' f; do
		if [ -f "$f" ]; then
			files+=("$f")
		fi
	done < <(git ls-files -z '*.go'; git ls-files -z --others --exclude-standard '*.go')
	if [ "${#files[@]}" -eq 0 ]; then
		return 0
	fi
	out=$(gofmt -l "${files[@]}") || return 2
	if [ -n "$out" ]; then
		printf '%s\n' "$out"
		return 1
	fi
	return 0
}

# The default lint and test commands are built lazily, so stub runs in
# scripts/gate_test.sh never touch envtest or make.
lint_default() {
	if [ -n "$LINT_GOTOOLCHAIN" ]; then
		GOTOOLCHAIN="$LINT_GOTOOLCHAIN" make lint-all
	else
		make lint-all
	fi
}

# test_default runs the suite directly, as scripts/test-affected.sh does, so
# no path is re-parsed by eval. A failed envtest lookup fails the step with
# its own message instead of running go test with empty KUBEBUILDER_ASSETS.
test_default() {
	: "${ENVTEST_K8S_VERSION:=$(go list -m -f '{{.Version}}' k8s.io/api | awk -F'[v.]' '{printf "1.%d", $3}')}"
	local assets
	if ! assets="$("${ENVTEST_BIN:-bin/setup-envtest}" use "$ENVTEST_K8S_VERSION" --bin-dir "${BIN_DIR:-$PWD/bin}" -p path)" ||
		[ -z "$assets" ]; then
		echo "gate: setup-envtest found no assets for $ENVTEST_K8S_VERSION"
		return 1
	fi
	# shellcheck disable=SC2046 # one argument per package, split on purpose
	KUBEBUILDER_ASSETS="$assets" go test -timeout "${GO_TEST_TIMEOUT:-20m}" $(go list ./... | grep -v '/test/e2e$')
}

step_regen() { eval "$GATE_REGEN_CMD"; }
step_fmt() {
	if [ -n "$GATE_FMT_CMD" ]; then eval "$GATE_FMT_CMD"; else fmt_check; fi
}
# The copyright step sits before vet because a stale header is fixed in one
# edit, while the lint and test round it would otherwise precede costs minutes.
step_copyright() {
	if [ -n "$GATE_COPYRIGHT_CMD" ]; then eval "$GATE_COPYRIGHT_CMD"; else make check-copyright; fi
}
step_vet() { eval "$GATE_VET_CMD"; }
step_lint() {
	if [ -n "$GATE_LINT_CMD" ]; then eval "$GATE_LINT_CMD"; else lint_default; fi
}
step_test() {
	if [ -n "$GATE_TEST_CMD" ]; then eval "$GATE_TEST_CMD"; else test_default; fi
}

run_logged() {
	local name="$1"
	shift
	"$@" >"$GATE_DIR/$name.log" 2>&1
}

status_paths() {
	git status --porcelain -uall | awk '{print substr($0, 4)}' | sort
}

# drift_check fails when regeneration modified a file the working tree had
# not already modified: the committed generated output would not match what
# a clean checkout builds from. $1 is the pre-regeneration snapshot.
drift_check() {
	local after drift
	after=$(status_paths)
	drift=$(comm -13 <(printf '%s\n' "$1") <(printf '%s\n' "$after"))
	if [ -n "$drift" ]; then
		{
			echo "regeneration drift: make manifests generate changed files the working tree had not already modified:"
			printf '%s\n' "$drift"
			echo "run make manifests generate and commit the regenerated files, then rerun the gate"
		} >&2
		return 1
	fi
	echo "no regeneration drift"
}

failed=0
before=$(status_paths)

if run_logged regen step_regen; then rc_regen=0; else rc_regen=$?; failed=1; fi
if [ "$rc_regen" -eq 0 ]; then
	if drift_check "$before" >"$GATE_DIR/drift.log" 2>&1; then rc_drift=0; else rc_drift=$?; failed=1; fi
else
	rc_drift=""
fi

if [ "$failed" -eq 0 ]; then
	if run_logged fmt step_fmt; then rc_fmt=0; else rc_fmt=$?; failed=1; fi
else
	rc_fmt=""
fi
if [ "$failed" -eq 0 ]; then
	if run_logged copyright step_copyright; then rc_copyright=0; else rc_copyright=$?; failed=1; fi
else
	rc_copyright=""
fi
if [ "$failed" -eq 0 ]; then
	if run_logged vet step_vet; then rc_vet=0; else rc_vet=$?; failed=1; fi
else
	rc_vet=""
fi

# Lint and tests run concurrently as shell background jobs, never as make -j
# across targets: targets that share the manifests/generate prerequisites
# would race the regeneration. wait's status is captured with if/else because
# set -e would otherwise exit on the first failing wait and mask the other.
rc_lint=""
rc_test=""
if [ "$failed" -eq 0 ]; then
	( step_lint ) >"$GATE_DIR/lint.log" 2>&1 & lint_pid=$!
	( step_test ) >"$GATE_DIR/test.log" 2>&1 & test_pid=$!
	if wait "$lint_pid"; then rc_lint=0; else rc_lint=$?; failed=1; fi
	if wait "$test_pid"; then rc_test=0; else rc_test=$?; failed=1; fi
fi

state() {
	if [ -z "$1" ]; then
		printf 'SKIPPED'
	elif [ "$1" -eq 0 ]; then
		printf 'PASS'
	else
		printf 'FAIL'
	fi
}
summary_line() {
	local log="$GATE_DIR/$1.log"
	if [ -z "$2" ]; then
		log=none
	fi
	printf 'gate: %-6s %-8s exit=%-4s log=%s\n' "$1" "$(state "$2")" "${2:-n/a}" "$log"
}
summary_line regen "$rc_regen"
summary_line drift "$rc_drift"
summary_line fmt "$rc_fmt"
summary_line copyright "$rc_copyright"
summary_line vet "$rc_vet"
summary_line lint "$rc_lint"
summary_line test "$rc_test"
if [ "$failed" -eq 0 ]; then
	echo "gate: PASS"
else
	echo "gate: FAIL"
	exit 1
fi

#!/usr/bin/env bash
#
# Regression test for envtest control-plane sharing in internal/controller
# (#2141).
#
# A control plane is an etcd plus a kube-apiserver, about 4 s to start. Before
# this test existed, 63 tests in internal/controller each started a private
# one, so the package ran at go test's 10 minute timeout on CI runners and the
# Tests job failed at random. The fix is one envtest.Environment per CRD set,
# started lazily and stopped by TestMain, with each test working in a
# generated namespace (internal/controller/envtest_shared_test.go).
#
# The rule this enforces: internal/controller's tests construct an
# envtest.Environment only in envtest_shared_test.go (the helpers) and
# suite_test.go (the Ginkgo suite). A test that writes its own bypasses the
# sharing and the TestMain budget, so it fails here instead of on a runner
# eleven minutes into the job.
#
# Scoped to internal/controller on purpose. The other envtest packages finish
# well inside the limit and keep their own control planes: internal/watchcanary
# 7 s and pkg/cli 12 s, 100 to 171 times under the 20m limit. The rule would
# be churn there.
#
# Usage: scripts/check-envtest-sharing_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKG="$ROOT/internal/controller"

allowed=" $PKG/envtest_shared_test.go $PKG/suite_test.go "

offenders=""
while IFS= read -r file; do
	[[ -z "$file" ]] && continue
	case "$allowed" in
	*" $file "*) ;;
	*) offenders+="${file#"$ROOT"/}"$'\n' ;;
	esac
done < <(grep -rl --include='*_test.go' 'envtest\.Environment' "$PKG" || true)

if [[ -n "$offenders" ]]; then
	echo "FAIL: these tests construct an envtest.Environment of their own:" >&2
	printf '%s' "$offenders" >&2
	echo "Use sharedTestEnv (internal/controller/envtest_shared_test.go) and" >&2
	echo "newTestNamespace instead; one control plane per CRD set (#2141)." >&2
	exit 1
fi

echo "PASS: every internal/controller test shares the control planes in envtest_shared_test.go"

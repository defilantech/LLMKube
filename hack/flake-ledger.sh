#!/usr/bin/env bash
#
# flake-ledger: record test failures on main, in the merge queue, and failures
# that passed on a rerun of the same commit as one GitHub issue per flaky test
# (#2378). The logic is cmd/flake-ledger; this runs it from the repo root.
#
#   hack/flake-ledger.sh --since 7d --dry-run      # print what it would file
#   hack/flake-ledger.sh --since 3d                # file it (needs issues: write)
#
# --since takes 3d, 36h, a date (2026-10-20) or an RFC 3339 time; -v prints
# each comment body. GH_TOKEN or GITHUB_TOKEN is used when set, otherwise the
# gh CLI's login. .github/workflows/flake-ledger.yml runs this daily.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -z "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]] && command -v gh >/dev/null 2>&1; then
	GH_TOKEN="$(gh auth token)"
	export GH_TOKEN
fi
exec go -C "$repo_root" run ./cmd/flake-ledger "$@"

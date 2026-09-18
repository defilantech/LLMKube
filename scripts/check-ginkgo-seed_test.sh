#!/usr/bin/env bash
#
# Regression test for the GINKGO_SEED definition in the Makefile (#1817).
#
# `GINKGO_SEED ?= $(shell date +%s)` is a recursively expanded variable, so GNU
# Make re-runs `date` at every textual reference. The seed echoed in the
# `(replay: ...)` line and the seed exported to `go test` are then two separate
# invocations, so a run can log a seed it did not use. That log is exactly where
# a seed is read to replay a failure (#1693). The fix defines the variable once
# under an `ifndef` guard.
#
# This test expands the variable twice on one recipe line, around a
# `$(shell sleep 1)` that Make evaluates between them, and asserts the two
# values match. Make expands a recipe line left to right, so the references
# land a second apart: on the recursive `?=` they are two `date` calls and
# differ, on the fixed `:=` the value is computed once and they agree. It also
# asserts a command-line override is still honored.
#
# Usage: scripts/check-ginkgo-seed_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The single-quoted format keeps $(GINKGO_SEED) literal for Make; printf still
# expands \t and \n. `-f -` appends this fragment to the real Makefile, so the
# slow `test` target never runs.
seed_probe() {
  printf 'seed-probe:\n\t@echo "$(GINKGO_SEED) $(shell sleep 1) $(GINKGO_SEED)"\n' \
    | make -s -C "$ROOT" -f Makefile -f - seed-probe "$@"
}

out="$(seed_probe)"
bare_first="${out%% *}"
bare_second="${out##* }"
if [[ "$bare_first" != "$bare_second" ]]; then
  echo "FAIL: GINKGO_SEED evaluated twice; echoed=$bare_first exported=$bare_second" >&2
  exit 1
fi

out="$(seed_probe GINKGO_SEED=42)"
ov_first="${out%% *}"
ov_second="${out##* }"
if [[ "$ov_first" != "42" || "$ov_second" != "42" ]]; then
  echo "FAIL: GINKGO_SEED=42 override not honored once: $ov_first / $ov_second" >&2
  exit 1
fi

echo "PASS: GINKGO_SEED is evaluated once and the override is honored"

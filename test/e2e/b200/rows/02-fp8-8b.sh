#!/usr/bin/env bash
# Row 2: single-GPU 8B model FP8 (E4M3) serve.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_standard_row 2 b200-row-02 "$B200_MANIFESTS/02-fp8-8b.yaml" 5
}

#!/usr/bin/env bash
# Row 3: single-GPU 70B FP8 serve, single chassis.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_standard_row 3 b200-row-03 "$B200_MANIFESTS/03-fp8-70b.yaml" 2
}

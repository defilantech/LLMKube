#!/usr/bin/env bash
# Row 1: single-GPU small-model serve on B200 (FP16 baseline).
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

# vLLM, not llama.cpp: llama.cpp has no sm_100 codegen, so a llama.cpp
# baseline would measure the PTX-JIT Hopper path and misreport it as a
# Blackwell baseline (matrix row 1 correction, #1375).
b200_row_main() {
  b200_standard_row 1 b200-row-01 "$B200_MANIFESTS/01-small-serve.yaml" 5
}

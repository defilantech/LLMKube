#!/usr/bin/env bash
# Row 4: 8x B200 single-chassis multi-GPU sharding via NVLink5.
#
# Sourced by run-matrix.sh, which calls b200_row_main.
# shellcheck source=/dev/null
. "$B200_LIB/capture.sh"

b200_row_main() {
  b200_standard_row 4 b200-row-04 "$B200_MANIFESTS/04-nvlink-shard.yaml" 2
}

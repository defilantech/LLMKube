# B200 validation harness

Executable form of the NVIDIA Blackwell B200 (sm_100) validation matrix tracked
in [`docs/operations/b200-validation-matrix.md`](../../../docs/operations/b200-validation-matrix.md)
and issued as [#1376](https://github.com/defilantech/LLMKube/issues/1376).

The point is to spend a short, contended hardware window running tests rather
than authoring them. Each matrix row is a scripted command with a pre-staged
manifest, a preflight floor gate, and an asserted result.

## Layout

```
run-matrix.sh          # --rows 1,2,3,4 --out results/ [--dry-run] [--force]
rows/NN-*.sh          # one function per matrix row, sourced by run-matrix.sh
manifests/NN-*.yaml   # Model + InferenceService per row
lib/preflight.sh      # assert platform floors before any GPU row runs
lib/capture.sh        # normalize a benchmark result; the only writer of the
                      # matrix doc's status cells
lib/common.sh         # shared logging and the standard row driver
fixtures/             # floors and measured-version fixtures for the self-test
selftest.sh           # hardware-free suite; runs in CI
```

## Usage

```sh
# Validate the harness wiring without hardware.
test/e2e/b200/run-matrix.sh --rows 1 --dry-run

# Run rows 1 and 2 on a Blackwell node.
test/e2e/b200/run-matrix.sh --rows 1,2

# Re-run a row that already produced a result.
test/e2e/b200/run-matrix.sh --rows 1 --force
```

`--dry-run` validates the preflight self-check and the dry-run row shape
against fixtures, and never deploys. It is the only hardware-free path that
touches the row scripts; it is what makes the harness reviewable before B200
access lands.

Rows are individually runnable and idempotent. A row whose result file already
exists is skipped, so an interrupted hardware session resumes instead of
restarting. Pass `--force` to re-run.

## Preflight gate

Before any row runs, `lib/preflight.sh` asserts that the node meets the floors
in `charts/llmkube/values.yaml` (`platformFloors`): NVIDIA driver, CUDA
toolkit, NCCL, GPU operator, k8s device plugin, and DCGM exporter, plus that
the Fabric Manager version matches the driver exactly. A Fabric Manager
mismatch silently degrades NVLink5 to PCIe with no error surface, which is why
it is a gate rather than a row assertion.

A reading the harness cannot take is reported as unverified and fails the
gate. It is never treated as a pass.

## Row contract

Each row is `apply manifest -> wait Ready -> llmkube benchmark -o json ->
assert -> capture`. Rows 1-4 use the shared `b200_standard_row` driver in
`lib/common.sh` and assert a decode-throughput floor:

| Row | Model | Decode floor |
|---|---|---|
| 1 | TinyLlama-1.1B FP16 | 5 tok/s |
| 2 | Llama-3.1-8B-Instruct FP8 | 5 tok/s |
| 3 | Llama-3.1-70B-Instruct FP8 | 2 tok/s |
| 4 | Llama-3.1-70B-Instruct FP8, 8x shard | 2 tok/s |

The floors are conservative sanity gates, not performance targets. They exist
to catch a silent CPU fallback, where the server is healthy but decodes at a
tenth of the GPU rate.

Row 1 is vLLM on purpose. llama.cpp has no `sm_100` codegen, so a llama.cpp
baseline would measure the PTX-JIT Hopper path and misreport it as a Blackwell
baseline (matrix row 1 correction, #1375).

## Scope

Rows 1-4 are implemented here. Rows 5-10 (DCGM Blackwell counters, recording
rules under load, MIG profiles, NVFP4, MXFP4, operational runbooks) are a
follow-up pass and stay `blocked-by-hardware` until then. The preflight gate and
capture path they will reuse are already in place.

## Self-test

```sh
make test-b200-harness
```

Runs `selftest.sh`, which needs no cluster and no GPU. It covers the preflight
gate (compliant, below-floor, Fabric Manager mismatch), the capture round-trip,
the matrix-doc rewrite, and the dry-run resume guard.

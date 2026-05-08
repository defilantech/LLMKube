# NVIDIA Blackwell B200 (sm_100) validation matrix

This document tracks LLMKube's validation status on NVIDIA's Blackwell datacenter GPUs (B200, GB200, compute capability `10.0`, `sm_100`). The matrix names the concrete tests we will run when hardware access is reachable; each row is independently checkable and gets marked pass / fail / blocked as we work through it.

LLMKube is currently validated on H100, L4, and L40S. This document captures the planned coverage shift to make Blackwell B200 a first-class day-one target so enterprise users running LLMKube on next-generation NVIDIA hardware know what is tested and what is a known gap.

> **Tracking issue:** [#413](https://github.com/defilantech/LLMKube/issues/413). Filed sub-issues and sub-PRs link from the issue's checklist. Use this doc as the operational source of truth; the issue tracks status and discussion.

## Status summary

| Field | Value |
|---|---|
| Document version | 1 (initial) |
| Last updated | 2026-05-07 |
| Hardware access | Not currently reachable. Pending: NVIDIA dev partnership, customer install, or rented capacity. |
| Rows passing | 0 / 10 |
| Rows blocked on hardware | 10 / 10 |
| Concrete deltas already landed | None yet (see "Concrete deltas" below for the queue) |

When a row's status changes, update both this document and the tracking issue's checklist.

## Scope

**In scope.** Datacenter Blackwell on `sm_100`: B200 (single chassis, 8x B200 NVLink5 / NVSwitch5) and GB200 (Grace+Blackwell superchip, datacenter form factor).

**Out of scope here.** Consumer Blackwell (RTX 50-series, GB20x, `sm_120`) is similar but tracked separately. Multi-node B200 sharding (NVLink fabric across chassis) is a larger effort tracked outside this matrix; this document covers single-chassis topology only.

## Validation matrix

| # | Test | Status | Notes |
|---|---|---|---|
| 1 | Single-GPU TinyLlama-1.1B serve on B200 (FP16) | ⏳ blocked-by-hardware | Baseline sanity. Llama.cpp + GGUF Q4_K_M, default flags. Confirms driver, CUDA toolkit, GPU operator, and device plugin are wired. |
| 2 | Single-GPU 8B model FP8 (E4M3) serve | ⏳ blocked-by-hardware | Existing FP8 checkpoints (Llama-3.x-FP8, Qwen2.5-FP8) load unchanged on B200. Verify `vllm/vllm-openai` image runs the FP8 path under Blackwell Tensor Core Gen-5. |
| 3 | Single-GPU 70B FP8 serve, single chassis | ⏳ blocked-by-hardware | Memory-bound; exercises HBM3e bandwidth. End-to-end `llmkube deploy` on a single B200 with a 70B-class FP8 model. |
| 4 | 8x B200 single-chassis multi-GPU sharding via NVLink5 | ⏳ blocked-by-hardware | Validate layer-based offload across 8 GPUs and confirm NVSwitch5 topology shows up correctly to the device plugin and the runtime. |
| 5 | DCGM exporter scrape with Blackwell-native counters | ⏳ blocked-by-hardware | Confirm dcgm-exporter publishes NVLink5 per-link, FP4 / Tensor Core Gen-5 utilization, per-die power split, HBM3e bandwidth, and PCIe Gen6 link health. Update the LLMKube Grafana dashboard from #409 to surface the new counters. |
| 6 | Recording rules from #409 produce series under load | ⏳ blocked-by-hardware | TTFT, queue wait, request restart rate at FP8 throughput. Confirms the observability contract scales to Blackwell-class throughput. |
| 7 | MIG profile deploys: 1g.23gb, 2g.45gb, 7g.180gb | ⏳ blocked-by-hardware | Verify the device plugin advertises `nvidia.com/mig-1g.23gb`, `nvidia.com/mig-2g.45gb`, `nvidia.com/mig-7g.180gb`, and that LLMKube Pods land correctly when those resources are requested. May surface a CRD field gap if MIG profile selection turns out to be needed at Model spec level. |
| 8 | NVFP4 inference (vLLM 0.10+, ModelOpt-converted) | ⏳ blocked-by-hardware | Real path but ModelOpt is proprietary; document the conversion workflow as part of the row. Likely the test that takes longest because of the conversion step. |
| 9 | MXFP4 inference | ⏳ blocked-by-hardware | OCP standard FP4, broader vLLM support than NVFP4. Document the per-runtime support matrix as part of the row. |
| 10 | Crashloop / OOM / NVLink-degrade operational runbooks fire correctly | ⏳ blocked-by-hardware | End-to-end exercise of the operational runbooks under `docs/operations/runbooks/` against B200 hardware. Validates that triage signals (DCGM alerts, MemoryPressure events from #390, controller logs) actually surface what an operator needs to act on. |

### Status legend

- ⏳ blocked-by-hardware: Test is scoped and ready to run; waiting on a B200 system to be reachable.
- 🟡 in progress: Test is actively being run; see linked PR or run log.
- ✅ pass: Test ran and matched expectations on the platform versions documented in the row's notes.
- ❌ fail: Test ran and surfaced a defect; row links to a follow-up issue or PR.
- 🚫 deferred: Test was explicitly deferred (not just blocked); row notes why.

## Methodology

Each row is independently runnable when hardware is reachable. The general shape:

1. Provision a single-chassis DGX B200 (or equivalent) with the platform versions documented under "Concrete deltas" below. Confirm `nvidia-smi` reports `sm_100` devices and `Fabric Manager` is healthy.
2. Install LLMKube via Helm at the version pinned in the row (default: latest released minor). Capture the install command and Helm values for reproducibility.
3. Run the row's test path. Capture: `kubectl describe inferenceservice` output, controller logs, runtime container logs, DCGM scrape sample, and a representative latency / throughput measurement.
4. Update this matrix and the tracking issue. If the row failed, file a focused follow-up issue with the captured evidence and link both directions.

Treat each row's evidence pack as the artifact. We want the matrix to be reproducible by anyone with B200 access, not just the maintainer who ran it first.

## Concrete deltas to land before B200 production use

These are project changes the LLMKube codebase, Helm chart, or docs need to absorb. Tracked in detail in #413; sub-issues / sub-PRs link from there.

### 1. Driver, CUDA, and GPU operator floors documented

`charts/llmkube/README.md` should grow a "Tested platforms" section capturing:

- NVIDIA driver: **560.35.03 minimum**, 570.x recommended for DGX OS 7. 550.x explicitly does not support B200; 555.x is preview-only.
- CUDA toolkit: **12.8 minimum** (first public `sm_100` codegen, January 2025); 12.9+ recommended; forward target CUDA 13.x.
- `gpu-operator >= v24.6` (v24.9 / v25.x recommended); `k8s-device-plugin >= v0.15` (v0.17.x preferred).
- **Fabric Manager package version MUST match the NVIDIA driver exactly.** A mismatch silently degrades NVLink5 to PCIe with no clear error surface. Highest-priority silent-failure mode; deserves its own runbook entry under `docs/operations/runbooks/`.

### 2. DCGM exporter floor

- Floor: `dcgm-exporter 3.3.7` for basic Blackwell.
- Recommended: `4.0+` for Blackwell-native counters (NVLink5 per-link, Tensor Core Gen-5 utilization, per-die power split, HBM3e bandwidth, PCIe Gen6 link health).
- Update the Grafana dashboard from #409 to surface the new counters once 4.0 is in the bundled chart.

### 3. Runtime image bumps + sm_100 codegen

Audit the default images each runtime backend sets:

- `internal/controller/runtime_vllm.go`: `vllm/vllm-openai:v0.20.0` is likely fine (Blackwell support landed in 0.7.x, FP8 / MXFP4 mature in 0.9+). Confirm at validation time and record the exact tag the matrix passed against.
- `internal/controller/runtime_llamacpp.go`: `ghcr.io/ggml-org/llama.cpp:server` must be a build that includes `CMAKE_CUDA_ARCHITECTURES="90;100"`. Pin to a tag verified to ship `sm_100` codegen.
- `internal/controller/runtime_tgi.go`: `ghcr.io/huggingface/text-generation-inference:latest` should pin to a 3.x tag with rebuilt flash-attn for Blackwell.

### 4. FP8 / FP4 quantization in the CRD

`InferenceServiceSpec.VLLMConfig.Quantization` already exists. Verify and document:

- FP8 (E4M3, E5M2) values pass through cleanly.
- NVFP4 and MXFP4 are in the allowed value set.
- The field's GoDoc records the per-runtime support matrix.
- The model catalog docs name a conversion path for both NVFP4 (NVIDIA ModelOpt, proprietary) and MXFP4 (OCP standard).

### 5. NCCL version floor for multi-GPU

Document in the multi-GPU deployment guide:

- `NCCL >= 2.23` for NVLink5 / NVSwitch5 topology.
- `NCCL 2.24+` recommended; older versions show small-message all-reduce regressions on Blackwell.

### 6. OFED / OS floors for GPUDirect RDMA on Gen6

For the air-gapped install guide and any future multi-node sharding work:

- DGX OS 7 (Ubuntu 24.04, kernel 6.8+) or non-DGX hosts at kernel 6.5+.
- `MLNX_OFED 24.10+` or `DOCA-OFED 2.9+` for ConnectX-7 / BlueField-3 GPUDirect RDMA on PCIe Gen6.

## Common silent-failure modes

Each of these gets a runbook entry under `docs/operations/runbooks/` (filed as part of the broader operational runbook effort, not gated on B200 hardware):

1. **Fabric Manager / driver version mismatch.** NVLink5 degrades to PCIe with no clear error. Detection: NVLink bandwidth scrape from DCGM; alert if reported topology bandwidth diverges from expected.
2. **Old base image (CUDA 12.4 or earlier) on B200.** Kernels appear to load but fall back or fail at launch. Detection: container logs around model load; capture `cuobjdump --list-elf` for the runtime image.
3. **NCCL 2.21 or earlier on Blackwell.** Small-message all-reduce regressions. Detection: collective benchmark in row #4 of the matrix.
4. **Old OFED on Gen6.** GPUDirect RDMA disabled silently; looks like generic network slowness. Detection: `nvidia-smi topo --matrix` plus an RDMA performance benchmark.

## What this document is NOT

- A CRD redesign for Blackwell. The existing `Hardware.GPU.Count` field is sufficient; MIG profile selection is a follow-up if matrix row 7 shows it is needed.
- A multi-node sharding plan. That is a separate larger effort.
- A claim that LLMKube is officially "Blackwell-certified." This is a validation roadmap, not a marketing claim.

## References

- Tracking issue: [#413 enterprise readiness for NVIDIA Blackwell B200](https://github.com/defilantech/LLMKube/issues/413)
- Cross-cutting issues: [#409 (recording rules + dashboard)](https://github.com/defilantech/LLMKube/issues/409), [#390 (metal-agent K8s events)](https://github.com/defilantech/LLMKube/issues/390), [#53 (air-gapped deployment)](https://github.com/defilantech/LLMKube/issues/53)
- NVIDIA: [Blackwell architecture overview](https://www.nvidia.com/en-us/data-center/blackwell-architecture/), [DGX B200 datasheet](https://www.nvidia.com/en-us/data-center/dgx-b200/)
- vLLM Blackwell support tracking: [vllm-project/vllm](https://github.com/vllm-project/vllm) (search recent issues for `Blackwell`, `sm_100`, `B200`)

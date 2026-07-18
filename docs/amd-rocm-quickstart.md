# AMD ROCm Quickstart

This guide walks through deploying an LLM on an AMD GPU using the llama.cpp
ROCm (HIP) backend with LLMKube (issue #701). It is a per-model opt-in tier
alongside the existing [AMD Vulkan sample](../config/samples/model_amd_vulkan_igpu.yaml):
Vulkan remains the AMD default, and ROCm is something you choose for specific
workloads.

## When to Use ROCm vs Vulkan

Be honest about what ROCm buys you on Strix Halo (gfx1151) before switching a
model over. We measured both backends on the same node, same llama.cpp build
(b9663), same models (see [Benchmarks](#benchmarks)). The short version:

**Vulkan is the better default on Strix Halo across every case we measured.**
It wins raw decode, it degrades far more gracefully as context grows, it
addresses a much larger memory pool (about 112GB versus ROCm's 64GB carveout
on this driver stack, which decides whether a large model fits at all), and
it is competitive-to-faster once speculative decoding is in play. ROCm is
shipped as a fully supported, validated per-model option, but on this
hardware today it does not beat Vulkan. There is no free "ROCm is faster
inference" win on Strix Halo.

Where ROCm's numbers are actually stronger:

- **Zero-context prefill** (317 vs 287 tok/s) and, relatedly, raw
  **draft-verification speed** under speculative decoding. On a prompt where
  both backends reach the same draft-acceptance rate, ROCm's faster
  verification pulls ahead.

Where Vulkan wins:

- **Raw decode** at every context depth, and it holds up much better as
  context grows (32k: 8.72 vs 7.41 tok/s).
- **Prefill at real context.** ROCm's prefill collapses with depth while
  RADV's coopmat path scales (32k prefill: 130 vs 51 tok/s).
- **Memory ceiling.** RADV addresses the full unified pool (~112GB); ROCm/HIP
  sees only the BIOS VRAM carveout (~64GB) and does not spill into GTT by
  default. For a model larger than the carveout, Vulkan is the path on this
  stack (see [Large models](#large-model-note-memory-ceiling)).
- **Speculative decoding, net.** Decode rate under a draft or MTP is
  dominated by draft-acceptance, which varies by prompt; Vulkan reached
  higher acceptance on most prompts we tried (ROCm's TOP_K sampler currently
  runs on CPU, which appears to cost draft quality), so end to end it is a
  wash-to-Vulkan.

So: **default to Vulkan.** Choose ROCm when you specifically want the HIP
stack (to match other AMD/CDNA deployments, or to track ROCm maturity on
RDNA), and re-measure for your own model and prompt mix rather than assuming a
win. Use the [Benchmarks](#benchmarks) numbers, not vibes, to decide
per-model.

## Node Prerequisites

- Kubernetes cluster with an AMD GPU node and a generic-device-plugin
  (for example `squat/generic-device-plugin`) advertising the shared
  `devic.es/dri-render` resource described below.
- Linux kernel >= 6.18.4.
- `linux-firmware` >= 20260110. **20251125 is known-bad**; do not run it on a
  ROCm-tier node.
- `amdgpu.lockup_timeout=20000` kernel argument recommended. ROCm's own
  memory and kernel scheduling behave differently than Vulkan's under
  sustained load, and the default ~10s GPU reset timeout can trip a false
  "device lost" on long-context or high-concurrency runs. 20 seconds avoids
  the false positive without masking a genuine hang.
- LLMKube operator installed.
- `kubectl` configured for your cluster.

### Device plugin: one shared resource, not two

ROCm and Vulkan schedule against the **same** device-plugin resource,
`devic.es/dri-render`. There is no separate `devic.es/amd-rocm` resource.
The group backing `devic.es/dri-render` must include `/dev/kfd` (HIP's
device node) alongside `/dev/dri/renderD128` and a globbed
`/dev/dri/card*` (the card index can move across reboots):

```yaml
# generic-device-plugin DaemonSet arg (excerpt)
- --device
- |
  name: dri-render
  groups:
    - count: 4
      paths:
        - path: /dev/kfd
        - path: /dev/dri/renderD128
        - path: /dev/dri/card*
```

This is deliberate, not a shortcut. A node with one physical AMD GPU has
exactly one render device to hand out, so:

- **Single physical GPU.** There is only one device to advertise. A second
  resource would just be a second name for the same hardware.
- **Health-watch collisions.** `generic-device-plugin` runs an independent
  health check per advertised group. Two groups whose paths overlap
  (`renderD128`, `card*`) end up polling and locking the same device files
  from two goroutines, which is a source of spurious unhealthy/flapping
  device state.
- **GPU double-count.** If `devic.es/dri-render` and `devic.es/amd-rocm` were
  both advertised for the same physical GPU, the scheduler would see twice
  the allocatable GPU capacity that actually exists, and could place a
  Vulkan pod and a ROCm pod on the same GPU at once, expecting isolation
  that does not exist.

Verify the node advertises the resource and that `/dev/kfd` is actually
injected:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,DRI:.status.allocatable."devic\.es/dri-render"
```

Expected: a non-zero `devic.es/dri-render` value on the AMD GPU node.

```bash
kubectl run rocm-probe --rm -i --restart=Never --image=busybox:1.36 \
  --overrides='{"spec":{"tolerations":[{"key":"devic.es/dri-render","operator":"Exists"}],"containers":[{"name":"p","image":"busybox:1.36","command":["ls","-la","/dev/kfd","/dev/dri"],"resources":{"limits":{"devic.es/dri-render":"1"}}}]}}'
```

Expected: the listing includes both `/dev/kfd` and `/dev/dri/renderD128`.

## Selecting the ROCm Tier

Select ROCm per-model with `hardware.gpu.vendor: amd` and
`hardware.gpu.runtime: rocm` (the `llmkube` CLI does not yet expose the GPU
runtime selector; use a YAML manifest):

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen36-27b-rocm
  namespace: default
spec:
  source: https://huggingface.co/unsloth/Qwen3.6-27B-GGUF/resolve/main/Qwen3.6-27B-Q6_K.gguf
  format: gguf
  quantization: Q6_K
  hardware:
    accelerator: rocm
    gpu:
      enabled: true
      vendor: amd
      runtime: rocm
      count: 1
  resources:
    cpu: "2"
    memory: 8Gi
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen36-27b-rocm
  namespace: default
spec:
  modelRef: qwen36-27b-rocm
  replicas: 1
  contextSize: 32768
  flashAttention: true
  resources:
    cpu: "4"
    gpu: 1
    memory: 12Gi
  endpoint:
    type: ClusterIP
    port: 8080
    path: /v1/chat/completions
```

The full sample lives at
[`config/samples/model_amd_rocm_igpu.yaml`](../config/samples/model_amd_rocm_igpu.yaml).
Apply it directly:

```bash
kubectl apply -f config/samples/model_amd_rocm_igpu.yaml
```

### Escape hatch: AMD's official device plugin

If your cluster instead runs AMD's official ROCm device plugin (the one that
advertises `amd.com/gpu`, as covered in the
[Strix Halo quickstart](strix-halo-onboarding.md)), you are not forced onto
`devic.es/dri-render`. Set `hardware.gpu.resourceName: amd.com/gpu` on the
Model explicitly; an explicit `resourceName` always wins over the
vendor/runtime defaults.

### Behavior change from before #701

Before #701, `vendor: amd` with `runtime: rocm` (or `runtime` left unset)
fell through to the `amd.com/gpu` resource with the stock (non-HIP) image, so
it never actually served on ROCm. That fallthrough is **preserved** for an
unset `runtime` (back-compat with existing manifests targeting AMD's
official plugin). Setting `runtime: rocm` **explicitly** is what opts a Model
into the new shared `devic.es/dri-render` resource and LLMKube's ROCm image.
If you have existing AMD Models with `runtime` unset, they keep working
exactly as before; nothing changes until you add `runtime: rocm`.

## Verify Scheduling and HIP Offload

```bash
kubectl get model qwen36-27b-rocm -w
kubectl get inferenceservice qwen36-27b-rocm -w
kubectl get pods -l app=qwen36-27b-rocm -o wide
```

Check the pod requested `devic.es/dri-render` and got the matching
toleration:

```bash
kubectl get deploy qwen36-27b-rocm -o jsonpath='{.spec.template.spec.containers[0].resources.limits}{"\n"}{.spec.template.spec.tolerations}{"\n"}'
```

Confirm HIP/ROCm offload in the logs (llama.cpp's HIP build reuses its CUDA
backend code path, so the log lines say "ROCm devices" rather than "HIP
devices"):

```bash
POD=$(kubectl get pod -l app=qwen36-27b-rocm -o jsonpath='{.items[0].metadata.name}')
kubectl logs "$POD" -c llama-server --tail=200
```

Expected log markers:

- `found 1 ROCm devices`
- `offloaded .../... layers to GPU`

## Measure Token Throughput

```bash
kubectl port-forward svc/qwen36-27b-rocm 8080:8080
```

In another terminal:

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "messages": [{"role": "user", "content": "Explain Kubernetes in one sentence."}],
    "max_tokens": 128,
    "temperature": 0.2,
    "stream": false
  }' | jq '.timings.prompt_per_second, .timings.predicted_per_second'
```

`prompt_per_second` (prefill) is strongest for ROCm at low context but falls
off with depth; `predicted_per_second` (decode) is the bandwidth-bound number
where Vulkan is competitive or ahead. See [Benchmarks](#benchmarks) for the
measured comparison.

## Large-Model Note: memory ceiling

This is the one place ROCm is at a real disadvantage on Strix Halo, and it is
counterintuitive, so measure before you plan a large deployment.

On this driver stack the two backends do **not** see the same amount of
memory:

- **ROCm/HIP sees only the BIOS VRAM carveout** (about 64GB on our node) and
  does not spill into the shared GTT/system pool by default. `hipMalloc`
  stays inside the carveout.
- **Vulkan/RADV addresses the full unified pool** (about 112GB observed:
  VRAM carveout plus GTT) automatically.

So on the *same* 128GB machine, a model that needs more than the carveout
(roughly >60GB of weights plus KV cache) **fits under Vulkan but not under
ROCm** without extra work. This is why very large models (for example a
low-quant 218B) run on Strix Halo under Vulkan today. The 27B Q6_K example
above (~22GB weights plus KV) fits comfortably under either backend.

If you specifically need ROCm to address more than its carveout, it takes a
reboot and is a per-node opt-in, not a default:

- **Lower the BIOS UMA / VRAM carveout** (e.g. to the minimum) so the OS
  reclaims most of the 128GB, then raise `amdgpu.gttsize` and
  `ttm.pages_limit` so the iGPU can map the unified pool. `ttm.pages_limit`
  is in 4KB pages, so 12582912 pages is ~48GB; size it to the memory you want
  the GPU to reach, leaving headroom for the OS.
- Weigh whether it is worth it: for a model that big, Vulkan already reaches
  the memory today, and (per [Benchmarks](#benchmarks)) Vulkan is the faster
  backend on this hardware anyway. The ROCm-carveout retune mainly matters if
  you specifically need the HIP stack for that workload.

Do **not** enable `GGML_CUDA_ENABLE_UNIFIED_MEMORY` to work around the
carveout. It is presence-checked (any value, including `0`, turns it on), it
does not improve throughput when the model fits VRAM (we measured it slightly
slower), and on gfx1151 the managed-memory path can produce incoherent output.
Leave it unset.

Very large MoE models may additionally need a batch-size clamp to control
memory pressure during prefill; pass it via `extraArgs` on the
InferenceService, for example:

```yaml
extraArgs:
  - "-b"
  - "256"
```

## Benchmarks

Measured on Strix Halo (Radeon 8060S / Ryzen AI Max+ 395, gfx1151),
llama.cpp b9663, both backends built from the same commit, GPU to itself for
each run.

### Raw throughput

`llama-bench`, Qwen3.6-27B Q6_K, `-fa 1 -ngl 99 -p 512 -n 128 -d 0,8192,32768`,
tokens/sec:

| Context depth | Metric | ROCm | Vulkan | Winner |
|---|---|---:|---:|---|
| 0 | prefill (pp512) | **317** | 287 | ROCm +10% |
| 0 | decode (tg128) | 9.28 | **9.57** | Vulkan |
| 8k | prefill | 142 | **243** | Vulkan +71% |
| 8k | decode | 8.82 | **9.34** | Vulkan |
| 32k | prefill | 51 | **130** | Vulkan +155% |
| 32k | decode | 7.41 | **8.72** | Vulkan |

ROCm leads only on zero-context prefill; Vulkan wins decode at every depth and
its prefill scales far better with context. `GGML_CUDA_ENABLE_UNIFIED_MEMORY`
made no positive difference (pp512 275 vs 317, decode unchanged), so it is not
enabled.

### Speculative decoding (draft / MTP)

`llama-server --spec-type draft-mtp --spec-draft-n-max 12`, Qwopus3.6-27B Coder
MTP Q4, three coding prompts, decode tokens/sec:

| Backend | decode tok/s (range) | mean | draft acceptance |
|---|---|---:|---|
| ROCm | 14.4 - 16.5 | 15.3 | 22 - 26% |
| Vulkan | 11.8 - 21.7 | 16.9 | 21 - 45% |

Speculative decoding roughly doubles decode over the raw ~9 tok/s on both
backends. The end-to-end rate is dominated by draft-acceptance, which varies
by prompt. On a prompt where both backends hit the same acceptance, ROCm's
faster draft-verification wins (14.4 vs 11.8 at ~21% acceptance); but Vulkan
reached higher acceptance on the other prompts (ROCm's TOP_K sampler runs on
CPU, which appears to cost draft quality), so on average it is a
wash-to-Vulkan. If you rely on speculative decoding, benchmark your own draft
model and prompt mix; do not assume a backend win in either direction.

### Memory

At load, RADV/Vulkan reports ~112GB addressable (unified pool) versus ROCm's
~64GB (BIOS VRAM carveout). See
[Large-Model Note](#large-model-note-memory-ceiling).

## Troubleshooting

### Insufficient devic.es/dri-render

The device plugin is not running on the AMD GPU node, or its group config
does not include the node's render device. Check:

```bash
kubectl describe pod -l app=qwen36-27b-rocm
kubectl logs -n kube-system -l app.kubernetes.io/name=amd-dri-device-plugin
```

### /dev/kfd missing in the container

The `devic.es/dri-render` group config was not updated to include
`/dev/kfd` alongside the render node paths (see
[Node Prerequisites](#node-prerequisites)). Confirm with the probe pod shown
above; if `/dev/kfd` is absent from the listing, fix the device plugin's
group definition and restart it.

### GPU reset / "device lost" under sustained load

A spurious amdgpu reset kills the inference backend mid-run on long-context
or high-concurrency ROCm workloads. Raise the GPU lockup timeout with
`amdgpu.lockup_timeout=20000` and reboot; confirm it is live:

```bash
cat /proc/cmdline | tr ' ' '\n' | grep lockup_timeout
```

If resets persist afterward, the workload is genuinely hanging the GPU
rather than hitting a false timeout; check `dmesg | grep -i amdgpu` for the
failing ring and reduce context length, concurrency, or (for very large MoE)
the batch size.

### Model schedules but never reports ready

Confirm you set `runtime: rocm` explicitly rather than leaving it unset
(see [Behavior change from before #701](#behavior-change-from-before-701)):
an unset runtime targets `amd.com/gpu`, which is a no-op on a node that only
advertises `devic.es/dri-render`.

## See also

- [AMD Vulkan sample](../config/samples/model_amd_vulkan_igpu.yaml) and the
  [AMD/Vulkan runtime image proposal](proposals/697-amd-vulkan-runtime-image.md)
  the ROCm image build mirrors.
- [Strix Halo quickstart](strix-halo-onboarding.md): AMD's official
  ROCm GPU Operator path (`amd.com/gpu`), kernel args, and OOM/GTT guidance
  that also applies here.
- [Intel GPU quickstart](intel-gpu-quickstart.md): same per-vendor pattern,
  different accelerator.

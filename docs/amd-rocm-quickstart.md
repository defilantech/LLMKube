# AMD ROCm Quickstart

This guide walks through deploying an LLM on an AMD GPU using the llama.cpp
ROCm (HIP) backend with LLMKube (issue #701). It is a per-model opt-in tier
alongside the existing [AMD Vulkan sample](../config/samples/model_amd_vulkan_igpu.yaml):
Vulkan remains the AMD default, and ROCm is something you choose for specific
workloads.

## When to Use ROCm vs Vulkan

Be honest with yourself about what ROCm buys you before switching a model
over. On a single AMD iGPU/APU node:

- ROCm wins on **prefill (prompt processing)**, **long-context decode
  retention**, and **batched concurrency**. Attention and matmul-heavy
  workloads benefit from HIP's more mature kernels, and rocWMMA flash
  attention holds up better as context grows.
- **Vulkan stays the default.** Short-context decode on these nodes is
  memory-bandwidth-bound, not compute-bound, so the two backends are close
  and Vulkan usually wins that specific case. There is no free "ROCm is
  faster inference" win: pick the backend that matches your workload shape,
  not the newer-sounding one.
- In practice: reach for ROCm when you are running long prompts, large
  context windows, or many concurrent requests against the same model. Stay
  on Vulkan for short-prompt, low-concurrency chat traffic.

The [Benchmarks](#benchmarks) section below has (or will have) measured
numbers for a concrete model on this hardware; use them, not vibes, to decide
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

`prompt_per_second` is where ROCm's prefill advantage should show up most;
`predicted_per_second` (decode) is the bandwidth-bound number where Vulkan
is often competitive or ahead at short context.

## Large-Model Note: ttm.pages_limit

The default kernel GTT/UMA ceiling caps how much system memory the GPU can
address at once, which in practice limits GPU-visible memory to roughly
**62GB** on a Strix Halo-class unified-memory node. The 27B Q6_K example
above (~22GB of weights plus KV cache) fits comfortably under that ceiling;
**you do not need to touch `ttm.pages_limit` for it.**

If you deploy a model whose weights plus KV cache exceed that ceiling, raise
`ttm.pages_limit` (a kernel boot parameter; changing it requires a reboot) to
opt into a larger GPU-visible allocation. Treat this as an explicit,
per-node opt-in rather than a default: it trades system RAM headroom for GPU
addressability, and it is easy to over-commit a UMA node if you raise it
without also accounting for what else runs there.

Very large MoE models may additionally need a batch-size clamp to control
memory pressure during prefill; pass it via `extraArgs` on the
InferenceService, for example:

```yaml
extraArgs:
  - "-b"
  - "256"
```

## Benchmarks

Placeholder: measured numbers land once the validated ROCm-vs-Vulkan run is
complete (tracked in LLMKube#701). Do not treat the rows below as real data.

| Metric | Vulkan | ROCm | Notes |
|---|---|---|---|
| Prefill (prompt tok/s) | TBD | TBD | measured on Strix Halo gfx1151, llama.cpp b9663 |
| Decode, short context (tok/s) | TBD | TBD | measured on Strix Halo gfx1151, llama.cpp b9663 |
| Decode, long context (tok/s) | TBD | TBD | measured on Strix Halo gfx1151, llama.cpp b9663 |
| Concurrency (N parallel, aggregate tok/s) | TBD | TBD | measured on Strix Halo gfx1151, llama.cpp b9663 |

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

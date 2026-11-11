---
title: DeepSeek V4 Flash Vision on two DGX Sparks
description: A 157 GiB vision model served across two GB10 boxes as one InferenceService. Records the RoCE fabric setup, the vLLM tuning that moved throughput, the measured prefill and decode numbers with their KV cost, and the failures that cost the most time.
---

# DeepSeek V4 Flash Vision on two DGX Sparks

This build serves DeepSeek-V4-Flash-Vision-Exp across two NVIDIA DGX Spark
boxes as a single `InferenceService`, using `spec.multiNode`. It has been the
lab's agentic coding endpoint, so it is tuned for one long-context stream at a
time rather than for throughput under load.

The model does not fit one GB10. At 157 GiB of safetensors against 121.6 GiB
of unified memory per box, of which the runtime can address less again, a
single node is not close, and the usual escape hatch of a smaller quantisation
was not available for this release. Two nodes and a fast link were the only
route.

## Why this combination

The interesting part is not that a large model fits across two machines. It is
that a 200 Gb RoCE link between two 128 GB desktop-class boxes is enough to
serve a 157 GiB model at a usable interactive rate, which puts a class of model
inside reach of a lab that cannot buy a datacenter chassis.

Three findings drove the final configuration, and each is measured below:

- Speculative decoding is the single largest win, worth 46 to 58 percent on
  decode, and it costs far less KV cache than expected.
- The prefill chunk size, not the model or the fabric, was responsible for a
  10 percent prefill gap that looked like a multi-node overhead.
- The fabric is not the bottleneck at this size. Cross-node traffic is a few
  gigabytes per direction over a whole benchmark run.

## Hardware

Two of the lab's three DGX Sparks, `ahazidgx3` as rank 0 and `ahazidgx1` as
rank 1.

| Property | Value |
| --- | --- |
| SoC | NVIDIA GB10, compute capability `sm_121` |
| CPU | 20 cores, arm64 Grace |
| Memory | 121.6 GiB unified, shared between CPU and GPU |
| GPU count per node | 1 (`nvidia.com/gpu: 1`) |
| Disk | 916 GB, single volume |
| OS | Ubuntu 24.04.4 LTS, kernel `6.17.0-1031-nvidia` |
| NIC | ConnectX-7, 200 Gb/s per port, RoCE v2 |
| RDMA resource | `rdma/rdma_shared_device_a` |

Unified memory is the fact that shapes everything else. A GPU allocation is
system RAM, so a pod without a memory limit can take the node down rather than
being OOM-killed. Every serving pod here carries `resources.memory`.

### Fabric

The Sparks are cabled directly to each other, not through a switch. Each box
has two ConnectX-7 ports, and the ring is built by connecting port 0 of one
node to port 1 of the next.

| Leg | Node | Address | Ethernet device | RDMA device |
| --- | --- | --- | --- | --- |
| dgx3 to dgx1 | `ahazidgx3` | `10.10.4.1` | `enp1s0f0np0` | `rocep1s0f0` |
| dgx3 to dgx1 | `ahazidgx1` | `10.10.4.2` | `enp1s0f1np1` | `rocep1s0f1` |

Two details that are easy to get wrong:

- **The GID index is 3.** RoCE v2 exposes several GIDs per port and the wrong
  one fails at connection time with an error that reads like a cabling fault.
  Confirm with `show_gids` or `ibv_devinfo -v` before deploying, and set
  `spec.multiNode.ibGIDIndex` to match. Other labs report 5; do not copy it.
- **The link layer is Ethernet, not InfiniBand.** `ibv_devinfo` shows
  `link_layer: Ethernet` and `PORT_ACTIVE`. NCCL still uses the IB verbs path,
  which is what you want, but you have to tell it to: without
  `NCCL_NET=IB` and `NCCL_IB_DISABLE=0` it can quietly fall back to sockets and
  the group will work while running several times slower.

Set `NCCL_DEBUG=INFO` for the first bring-up. The line that confirms the fast
path is:

```
NET/IB : Using [0]rocep1s0f0:1/RoCE [RO]; OOB enp1s0f0np0:10.10.4.1
```

If you see `NET/Socket` instead, the transport is wrong regardless of whether
the model answers.

## The model artifact

| Property | Value |
| --- | --- |
| Model | `deepseek-ai/DeepSeek-V4-Flash-Vision-Exp` |
| Format | safetensors |
| Size on disk | about 157 GiB |
| Staging | one local-path PVC per member, both pinned to their node |

Each member needs its own copy on local disk. The claims are named per member
in the `InferenceService`, and the `Model` points at one of them.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: deepseek-v4-flash-vision-exp
spec:
  format: safetensors
  source: pvc://dsv4-vision-dgx3/snapshots/6821d6ad3681a4b137b066b76094fa82ebd0a380
  refreshPolicy: IfNotPresent
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      vendor: nvidia
      count: 1
```

### The Hugging Face cache layout trap

This one cost an afternoon. If you stage the model with `huggingface-cli` or
`hf download`, the result is a cache tree, and the files under
`snapshots/<hash>/` are **relative symlinks** into `../../blobs`. Mount the
snapshot directory as the volume root and every one of those links dangles.
The runtime's error does not mention symlinks; it says it cannot find a
recognised configuration file, which reads like a wrong path.

Mount the model **root** and put the hash in the path instead, exactly as the
`source` above does. All members must resolve the identical relative path, so
if one node's copy is laid out differently, fix the layout rather than the
path.

If you would rather not deal with the cache tree at all, stage from object
storage: `Model.spec.files` takes a list and pulls several artifacts from one
prefix, which is the cleaner route for sharded weights plus a projector or a
draft model.

## The InferenceService

The complete deployed spec, minus the two patch mounts described further down.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: dsv4-vision-ring
spec:
  modelRef: deepseek-v4-flash-vision-exp
  runtime: vllm
  image: vllm/vllm-openai:deepseekv4-flash-vision-arm64-cu130
  containerPort: 8080
  replicas: 1
  resources:
    gpu: 1
    cpu: "8"
    memory: 110Gi
  tolerations:
    - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
  vllmConfig:
    tensorParallelSize: 2
    pipelineParallelSize: 1
    enableExpertParallel: true
    kvCacheDtype: fp8_e4m3
    maxModelLen: 262144
    gpuMemoryUtilization: 0.80
  extraArgs:
    - --served-model-name
    - deepseek-v4-flash
    - deepseek-v4-flash-vision
    - --reasoning-parser
    - deepseek_v4
    - --tool-call-parser
    - deepseek_v4
    - --enable-auto-tool-choice
    - --max-num-seqs
    - "2"
    - --block-size
    - "256"
    - --speculative-config={"method":"dspark","model":"/model-source/snapshots/6821d6ad3681a4b137b066b76094fa82ebd0a380","num_speculative_tokens":3,"draft_sample_method":"probabilistic","enable_adaptive_verification":false}
  env:
    - {name: NCCL_DEBUG, value: INFO}
    - {name: VLLM_CACHE_ROOT, value: /cache/vllm}
    - {name: TRITON_CACHE_DIR, value: /cache/triton}
    - {name: TORCHINDUCTOR_CACHE_DIR, value: /cache/torchinductor}
    - {name: FLASHINFER_WORKSPACE_BASE, value: /cache/flashinfer}
    - {name: CUDA_CACHE_PATH, value: /cache/jit}
    - {name: HF_HUB_OFFLINE, value: "1"}
    - {name: PYTORCH_CUDA_ALLOC_CONF, value: expandable_segments:True}
  extraVolumes:
    - {name: cache, hostPath: {path: /var/lib/vllm-cache, type: DirectoryOrCreate}}
    - {name: shm, emptyDir: {medium: Memory, sizeLimit: 16Gi}}
  extraVolumeMounts:
    - {name: cache, mountPath: /cache}
    - {name: shm, mountPath: /dev/shm}
  multiNode:
    rdmaResource: rdma/rdma_shared_device_a
    ibGIDIndex: 3
    members:
      - node: ahazidgx3
        fabric: {address: 10.10.4.1, socketInterface: enp1s0f0np0, ibHCA: rocep1s0f0}
        modelCache: {claimName: dsv4-vision-dgx3}
      - node: ahazidgx1
        fabric: {address: 10.10.4.2, socketInterface: enp1s0f1np1, ibHCA: rocep1s0f1}
        modelCache: {claimName: dsv4-vision-dgx1}
```

The operator creates one pod per member, named `<service>-mn-<rank>`, and adds
the distributed arguments itself: `--nnodes`, `--node-rank`, `--master-addr`,
`--master-port`, `--distributed-executor-backend mp`, and `--headless` on the
non-zero ranks. Rank 0 serves the API. See the
[multi-node inference guide](../guides/multi-node-inference) for the API and
its failure semantics.

The spec above is the one this build ran. The parser triple in `extraArgs`
(`--reasoning-parser deepseek_v4`, `--tool-call-parser deepseek_v4`,
`--enable-auto-tool-choice`) now has a typed form,
`vllmConfig.reasoningParser` and `vllmConfig.toolCallParser`: the second
passes `--enable-auto-tool-choice` itself, so the pair replaces all three
lines. Leaving them in `extraArgs` still works, since `extraArgs` lands last.

The persistent cache mount at `/cache` matters more than it looks. It holds the
Triton, Inductor, FlashInfer and CUDA JIT caches, and on this platform the
upstream CUDA image ships no `sm_121` binaries, so everything is JIT compiled on
first use. Cold, that is about 18 seconds per pod plus 14 seconds on the first
request. Warm, it is nothing. Losing the cache directory turns every rollout
into a cold start.

### Two patches this build carries

The image is a first-party vLLM build for this model, and it needed two local
fixes at the time of writing. Both are mounted in rather than baked, so they can
be dropped when upstream catches up.

- A patched `sparse_mla_sm120_prefill.cu` from FlashInfer, mounted over the file
  in the installed package. Without it, TP2 prefill hits a dual-cache path that
  is not implemented for this architecture. The file is JIT compiled at runtime,
  which is what makes patching in place viable.
- An entrypoint wrapper, so the autotuner can be told to skip two operations
  that fail on this hardware, via
  `VLLM_FLASHINFER_AUTOTUNE_SKIP_OPS=trtllm_fp4_block_scale_moe,flashinfer::trtllm_fp4_block_scale_moe`.

## Measured results

All numbers from an in-cluster benchmark client, one stream at a time, 4,000
and 30,000 token prompts, 300 new tokens, temperature 0. The client path
matters: the same configuration measured through a port-forward from a laptop
read about 10 percent lower on prefill.

| Variant | 4k prompt prefill / decode | 30k prompt prefill / decode | KV cache tokens |
| --- | --- | --- | --- |
| A: baseline, no draft model | 1,870 / 25.3 | 1,915 / 25.0 | 1,768,361 |
| B: speculative decoding k=3 | 1,820 / 37.0 | 1,888 / 39.4 | 1,132,222 |
| C: B plus 8,192 prefill chunk | 2,107 / 37.3 | 1,985 / 38.0 | 333,071 |

Throughput is tokens per second, medians of three runs at 4k and a single run
at 30k. **Variant B is what the lab runs.**

Reading the table:

- **Speculative decoding is the win.** DSpark with three speculative tokens
  raises single-stream decode from 25 to between 37 and 39 tokens per second,
  46 to 58 percent, for a 36 percent reduction in KV cache. That is a far better
  trade than the halving that was expected, and two concurrent full-context
  streams still fit. Leave the maximum draft length unset; forcing it to 5
  collapsed the acceptance rate.
- **The prefill chunk was a red herring.** An earlier hand-built pair measured
  about 2,089 tokens per second of prefill, and the operator-managed group read
  10 percent lower. The difference was entirely
  `--max-num-batched-tokens 8192`, which the hand-built version set and the
  default does not. Variant C reproduces the old number.
- **That chunk is not worth it here.** It costs another 0.8M tokens of KV cache
  on top of the draft model, taking the service from two full-context streams to
  one. For a coding agent, which is decode-bound with a very high prefix cache
  hit rate, decode matters and prefill does not. A cluster serving short prompts
  under concurrency should choose differently.
- **KV cache dtype is doing heavy lifting.** `fp8_e4m3` is what makes a 262,144
  token context viable at all on this memory budget.

Fabric use during a full three-run benchmark was about 9.7 GB per direction,
and about 2.9 GB per direction to load and profile. At this model size the
200 Gb link is not the constraint.

Other measured facts:

- Head pod ready in 181 seconds from warm page cache.
- Killing the rank 1 worker by hand: the operator recreates the group and
  service is restored in about 258 seconds.
- Vision works through the same endpoint. The service correctly identified a
  generated red PNG at 211 prompt tokens.

## What went wrong

The failures were more expensive than the configuration, and most were not
about the model.

**A pod with no memory limit wedges the node.** Unified memory means a GPU
allocation is system RAM. A hand-written serving pod with requests but no
limits took two different Sparks down hard: the box still answered ping but
`sshd` was gone. Set `resources.memory` on anything that serves. The operator
does this for you; hand-rolled pods bypass it.

**Staging weights past 85 percent disk deletes your images.** The kubelet
image garbage collector fires at 85 percent and evicts every image not
currently in use, including the multi-gigabyte runtime you are about to
restart. On a single 916 GB volume holding two 157 GiB model copies plus
images, this is easy to hit. Budget the disk before staging and remove old
copies first.

**The group can be destroyed the moment it becomes healthy.** An early version
of the operator computed the group's identity hash from a rendered pod that
included an annotation only present while the service was not ready. The hash
changed at the instant of readiness, the group was judged stale, and it was
torn down two seconds after its first successful health check. Fixed before
release, but it is the shape of bug to expect in this area: any identity hash
over a multi-node group must be blind to status.

**Speculative decoding needs the right draft type.** This model's GGUF exports
carry no MTP layers, so the obvious `mtp` method crash-loops. The working
configuration is the `dspark` method pointed at the same weights.

**Do not confuse this with the llama.cpp RPC path.** A separate experiment
serving DeepSeek over llama.cpp's RPC workers fails with
`create_node invalid data ptr` on two or more workers. That is an upstream
llama.cpp issue, it looks exactly like a fabric fault, and it is unrelated to
the vLLM multi-node path documented here.

## Reproducing this

1. Cable the two nodes port 0 to port 1, confirm `PORT_ACTIVE` and find your
   GID index.
2. Install the RDMA device plugin so `rdma/rdma_shared_device_a` is allocatable.
3. Create one local-path PVC per node, sized for the weights plus headroom
   under 85 percent of the disk.
4. Stage the weights to both, keeping the relative layout identical.
5. Apply the `Model`, then the `InferenceService`.
6. Watch for the `NET/IB` line before trusting any number you measure.

A shorter starting point is
`config/samples/inferenceservice_multinode_vllm_two_sparks.yaml` in the repo.

## Related

- [Multi-node inference](../guides/multi-node-inference), the API reference.
- [KV cache types](/docs/operations/kv-cache), for the `fp8_e4m3` trade.
- [Model cache](../guides/model-cache), for staging strategies.

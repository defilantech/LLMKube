---
title: Multi-node inference
description: Serve one model across several nodes from a single InferenceService. Covers the spec.multiNode API, per-member fabric and storage, RDMA prerequisites, failure semantics, and the measured two-Spark vLLM configuration.
---

# Multi-node inference

A model that does not fit one node can still be one `InferenceService`.
`spec.multiNode` names the nodes, the operator runs one runtime process on
each, and the processes cooperate over the fabric between them as a single
serving group. Rank 0 answers requests; the other ranks are headless workers.

This is the shape for a DGX Spark ring, for a pair of DGX B200 chassis, and
for any cluster where the weights only fit across machines. It is not a
replacement for tensor parallel inside one chassis: a model that fits eight
GPUs on one node stays a single pod with `tensorParallelSize: 8`.

Supported runtime in this release: vLLM.

## The spec

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: dsv4-vision-ring
spec:
  modelRef: deepseek-v4-flash-vision-exp
  runtime: vllm
  image: vllm/vllm-openai:deepseekv4-flash-vision-arm64-cu130
  resources: {gpu: 1, memory: 110Gi, cpu: "8"}   # per member
  vllmConfig:
    tensorParallelSize: 2
    enableExpertParallel: true
    kvCacheDtype: fp8_e4m3
    maxModelLen: 262144
    toolCallParser: deepseek_v4
    reasoningParser: deepseek_v4
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

`members` is the rank order. The first entry is rank 0: it serves the
endpoint, keeps the labels the Service and the PodMonitor select on, and
carries the probes. Every other member runs `--headless` and joins the
torch.distributed rendezvous at rank 0's fabric address.

`resources` is per member. The world size is `tensorParallelSize x
pipelineParallelSize` and must equal `members x resources.gpu`: every GPU in
the group is a rank. Leave both sizes unset and the operator derives tensor
parallel across the whole group.

`toolCallParser` and `reasoningParser` are free-form values from the vLLM
image's parser registry. Setting `toolCallParser` also passes
`--enable-auto-tool-choice`, because vLLM ignores a parser without it. The two
names come from separate registries and often differ: DeepSeek V4 uses
`deepseek_v4` for both, GLM uses `toolCallParser: glm47` with
`reasoningParser: glm45`.

## Why fabric settings are per member

On a point-to-point ring every leg is its own subnet, and the same physical
cable has a different interface name and RDMA device name at each end. So
the NIC that carries the bootstrap sockets and the HCA that NCCL should use
are properties of the member, not of the service:

| Field | Becomes | Notes |
|---|---|---|
| `fabric.address` | `VLLM_HOST_IP`, and `--master-addr` for everyone when on rank 0 | required on rank 0 |
| `fabric.socketInterface` | `NCCL_SOCKET_IFNAME`, `GLOO_SOCKET_IFNAME`, `TP_SOCKET_IFNAME` | bootstrap only; data goes over RDMA |
| `fabric.ibHCA` | `NCCL_IB_HCA` | exact device (`rocep1s0f0`) or prefix (`mlx5`) |
| `ibGIDIndex` | `NCCL_IB_GID_INDEX` on every member | 3 is RoCE v2 on ConnectX-7 |
| `rendezvousPort` | `--master-port` | default 29500 |

User `env` on the service is applied after these, so an override still wins.

## RDMA prerequisites

NCCL over RoCE or InfiniBand from an unprivileged pod needs two things:
a device plugin that advertises the HCA as an extended resource, and
`CAP_IPC_LOCK` so NCCL can pin registered memory. Set `rdmaResource` to the
plugin's resource name and the operator requests one per member and adds
the capability. Members run with `hostNetwork` and `hostIPC`.

Check the head's log after startup. This line means RDMA is in use:

```
NCCL INFO NET/IB : Using [0]rocep1s0f0:1/RoCE
```

`NET/Socket` instead means NCCL fell back to TCP. It works, but on the
Spark ring prefill drops about 2.7x. Fix the resource, the capability, or
the GID index before benchmarking anything. NCCL prints that line only at
`NCCL_DEBUG=INFO`; at `WARN` the log is silent about the transport, so set
INFO for the first start and drop it afterwards.

## Storage

Every member needs the weights on its node. With a ReadWriteMany class,
`spec.modelCache.claimName` alone serves all members. Without one, give each
member its own claim through `members[i].modelCache.claimName`; the claim
replaces the service claim in that member's pod. All claims are user-owned
and must exist before the service is created; a missing one fails the
service with a message naming the member.

The operator swaps only the claim name, never the path, so the Model's path
inside the claim must be identical on every member. Two things bite here.
Weights staged by the Hugging Face hub live under
`models--<org>--<name>/snapshots/<hash>/`, where every file is a relative
symlink into `../../blobs/`; a volume rooted at the snapshot directory
mounts none of those targets and vLLM reports no configuration file. Root
the volume at the `models--<org>--<name>` directory and use
`pvc://<claim>/snapshots/<hash>` as the Model source. And a node that holds
a plain directory needs the same relative path, which a relative symlink
inside the volume root provides (`snapshots/<hash> -> ../<dir>`); a symlink
to a path outside the volume root dangles the same way.

## Node memory before boot

On a unified-memory host such as a DGX Spark, host RAM is GPU memory. Staging
a checkpoint leaves most of it as page cache, and a vLLM group whose KV cache
is pinned with `--kv-cache-memory-bytes` skips memory profiling, so nothing
notices that the cache is in the way until the allocator runs out.
`spec.multiNode.memory` has the operator clear it on every member, on every
boot:

```yaml
  multiNode:
    memory:
      dropPageCacheBeforeBoot: true
      sysctls:
        vm.min_free_kbytes: "1048576"
        vm.watermark_scale_factor: "200"
```

Either field adds one init container, `multinode-memory-prep`, as the last
init container of every member pod: it runs after the member's weights are
staged and right before its runtime starts. It writes the sysctls first, then
syncs and drops the page cache, and prints each value before and after:

```bash
kubectl logs <service>-mn-0 -c multinode-memory-prep
```

What to know before turning it on:

- **It is privileged.** `/proc/sys` is writable only in a privileged
  container, so the namespace's Pod Security level must allow privileged
  pods (a multiNode group already needs `hostNetwork` and `hostIPC`). Anyone
  who may create an InferenceService in that namespace can set these fields.
- **The writes are node-wide and stay.** The sysctls are not restored when
  the group is deleted, and they affect every pod on the node. The drop
  empties the page cache for everything on the node, which is why it suits a
  node dedicated to the group.
- **Only `vm.*` names with integer values** are accepted, at most 16. The API
  server rejects anything else.
- **Changing the block recreates the group**, like any other member change.
  Adding it to a serving group is a restart; a group without the block is
  untouched by an operator upgrade.
- **It covers boot, not serving.** A page cache that regrows under load still
  needs a serving-phase flusher; `config/samples/gb10-memory-guard.yaml` is
  the one the three-Spark ring runs.

## Failure semantics

The group is Ready only when every member is Running and rank 0 passes its
readiness probe. Any member that fails, restarts, is terminating, or carries
a stale spec deletes the whole group, and the next reconcile recreates it.
A rank 0 that stays Running but not Ready for 30 minutes counts as failed.
There is no rolling replacement: the ranks share one process group, so a
partial group cannot serve, and a recreate costs a full weight load on every
member (two to eight minutes measured on the Spark ring).

Any change to `spec.multiNode`, the model, the image, or the runtime config
recreates the group as well. `replicas` must be 1 or unset; autoscaling does
not apply. A `spec.autoscaling` block on a multiNode service is ignored: the
operator creates no HorizontalPodAutoscaler for it, deletes one it created
earlier, and reports `AutoscalingReady=False` with reason
`UnsupportedForMultiNode` without failing the service.

### Bounded recreates

A recreate replaces every member pod, so the kubelet never gets to put a
crashing container into `CrashLoopBackOff`, and the member pods always show
`RESTARTS 0`. The operator keeps the count instead, in
`status.multiNode.recreates`, and bounds it:

- A member failure (a runtime restart, a failed pod, an init container loop,
  or a rank 0 that is not Ready inside its 30 minute budget) tears the group
  down and counts as one failure. A spec change or a deleted member pod does
  not count.
- The group is recreated after a backoff: 30 seconds after the first failure,
  doubling for each further consecutive failure, capped at 5 minutes. While
  it waits the `MultiNodeGroupReady` condition has reason `BackOff`.
- The fourth consecutive failure (after three recreates) is terminal. The
  group is deleted and not recreated, the service goes to phase `Failed`, and
  the `Degraded` condition names the member and carries the failure.
- The count resets once the group has stayed Ready for 10 minutes, so a node
  reboot or a one-off OOM kill that recovers on retry never adds up to a
  failure. Changing the spec also starts a fresh count.
- To retry a `Failed` group without changing the spec, set `spec.suspend:
  true` (or `replicas: 0`) and then back.

Every counted failure also records a `MultiNodeGroupRecreating` Warning event
on the InferenceService, and reaching the bound records
`MultiNodeGroupFailed`, so `kubectl describe isvc` shows the history even
though the pods it describes are gone:

```yaml
status:
  phase: Failed
  multiNode:
    recreates:
      count: 4
      reason: MemberRestarted
      message: >-
        dsv41-ring-mn-0 restarted 1 time(s) (container vllm exit 1 Error:
        ...ValueError: No available memory for the cache blocks. Try
        increasing gpu_memory_utilization when initializing the engine....)
```

The failure message is an excerpt of the container's termination message,
which for a member is the end of its log. The excerpt is the first exception
line in it (the root cause of a Python traceback comes before the exceptions
that wrap it and before the shutdown warnings that end the log), else the
first line that reports an error, else the start and end of the message.

Status shows each rank:

```yaml
status:
  multiNode:
    size: 2
    readyMembers: 2
    members:
      - {rank: 0, node: ahazidgx3, pod: dsv4-vision-ring-mn-0, phase: Running, ready: true}
      - {rank: 1, node: ahazidgx1, pod: dsv4-vision-ring-mn-1, phase: Running}
  conditions:
    - type: MultiNodeGroupReady
      status: "True"
      reason: AllMembersRunning
```

Member pods are named `<service>-mn-<rank>` and carry the labels
`inference.llmkube.dev/multinode-group` and
`inference.llmkube.dev/multinode-rank`.

## Measured configurations

Two DGX Sparks (GB10, 121 GiB usable each) over one ConnectX-7 RoCE leg,
2026-09-02, single-stream agentic prompts:

| Model | Topology | Prefill tok/s | Decode tok/s |
|---|---|---|---|
| DeepSeek-V4-Flash-Vision-Exp (MXFP4) | vLLM TP2 + expert parallel | 2,089 | 24.0 |
| DeepSeek-V4-Flash-Vision-Exp, DSpark draft k=3 | vLLM TP2 + expert parallel | 2,047 | 39.1 |
| Qwen3.8-Flash-Next (NVFP4), MTP k=3 | vLLM TP2 + expert parallel | 2,789 | 48.1 |
| DeepSeek-V4-Flash (MXFP4) | llama.cpp RPC, two nodes | 357 | 19.4 |

The same model as an operator-managed `multiNode` group, benchmarked from
inside the cluster (median of three 3.7k-token prompts, then one 26k-token
prompt):

| Group configuration | Prefill tok/s (3.7k / 26k) | Decode tok/s (3.7k / 26k) | KV cache tokens |
|---|---|---|---|
| vLLM TP2 + expert parallel | 1,870 / 1,915 | 25.3 / 25.0 | 1,768,361 |
| plus DSpark draft k=3 | 1,820 / 1,888 | 37.0 / 39.4 | 1,132,222 |
| plus DSpark and `maxNumBatchedTokens: 8192` | 2,107 / 1,985 | 37.3 / 38.0 | 333,071 |

Two things to take from that table. The draft is where the win is for a
single agentic stream: decode rises by half for a third of the KV cache. And
the prefill chunk size is a trade, not a free setting: 8192-token chunks
match the hand-tuned prefill number but cost another 800k tokens of cache,
which decides how many long-context requests can run at once. The group
recreates in four to five minutes when a member dies, measured by deleting
the worker pod by hand.

Cross-node tensor parallel beat pipeline parallel on decode and tied it on
prefill; both models refuse pipeline parallel anyway. Three nodes did not
help single-stream latency: the third node adds a hop without adding work
per token.

For a pair of DGX B200 chassis the same spec reads `tensorParallelSize: 8`,
`pipelineParallelSize: 2`, two members with `ibHCA: mlx5` and `resources:
{gpu: 8}`.

## Three-Spark ring

Three DGX Sparks cabled directly (Port0 of each to Port1 of the next, NVIDIA's
`connect-three-sparks` playbook) form three point-to-point legs and no switch.
Its default leg addresses sit in 192.168.0.0/24 to 192.168.5.0/24; if the
management LAN is 192.168.1.0/24, as in the sample, move the legs elsewhere
(this lab uses 10.10.0.0/30 to 10.10.5.0/30) before the bootstrap can even
start. That changes two things about `spec.multiNode` that the field comments
state too gently.

**`fabric.address` is the bootstrap plane, and on a ring no fabric address
will do.** Rank 0's address is what every other rank dials for the torch
rendezvous, and each rank's address becomes its `VLLM_HOST_IP`. On a ring each
/30 is reachable from one neighbour only, so use the management IP and name
the management NIC in `socketInterface`. Bandwidth is irrelevant here; the
bootstrap is a handful of small messages.

**`ibHCA` is the data plane and takes a comma list.** List every ConnectX port
on the member (a Spark has four, named rocep1s0f0, rocep1s0f1, roceP2p1s0f0
and roceP2p1s0f1: two ConnectX controllers with two ports each) and set
`NCCL_IB_SUBNET_AWARE_ROUTING=1` in `spec.env`. NCCL >= 2.30 then picks, per
peer, the local port whose subnet matches that peer's. Without it NCCL assigns
the NIC by channel index on both ends of every link, and a P0->P1 ring is an
odd cycle that no index assignment can satisfy: every channel is wrong on one
end and the group dies with `ncclSystemError` during channel setup.
`NCCL_IB_MERGE_NICS=0` and `NCCL_NET_PLUGIN=none` complete NVIDIA's ring
environment; do not set `NCCL_IB_ADDR_RANGE` or `NCCL_CROSS_NIC`.

Measured 2026-09-13 with a 256 MB all-reduce over three ranks: 23.2 GB/s bus
bandwidth on the ring (11.3 GB/s for two ranks on one leg; 7.9 GB/s with
NCCL's Socket transport routed over the same legs, the fallback if RDMA is
unavailable). The full serving example, DeepSeek-V4.1-Flash EXL3 3.5bpw at
TP3, is
`config/samples/inferenceservice_multinode_vllm_three_sparks_ring.yaml`, with
its Model and the two checkpoint-side Jobs (the TP3 `config.json` edit, and a
page-cache drop before the group boots) next to it. The build write-up for it,
with the hardware table, the Engram-on-disk step and the failures in order, is
[DeepSeek-V4.1-Flash EXL3 on three DGX Sparks in a ring](../labs/deepseek-v41-flash-exl3-three-sparks-ring);
this guide stays the API reference, that page is the recipe.
Text-only, single stream,
on that ring: prefill 1,094 to 1,334 tok/s from 3.4k to 49.8k prompt tokens,
decode 26.5 tok/s without speculative decoding and 31 to 35 tok/s with DSpark
k=5 (65 tok/s on code, 1.5x across a mixed set; block verification is
lossless). The sample runs DSpark k=5 with a 131,072-token context and four
sequences, which leaves a KV pool of about 356,000 tokens at
`gpuMemoryUtilization: 0.78`.

Five operational notes from bringing it up: every member needs all 54
checkpoint files locally, including the two 101 GB Engram tables the runtime
reads from NVMe; a claim the operator must stage into has to be a pre-bound
static `local` PV (the members are placed with `nodeName`, so a
WaitForFirstConsumer class never binds); and after a large stage, drop the
page cache on each member before the group boots
(`multiNode.memory.dropPageCacheBeforeBoot`, see
[Node memory before boot](#node-memory-before-boot)), or the worker
refuses to start with CUDA-free below `gpuMemoryUtilization` x total. Turn
swap off on every member first (`sudo swapoff -a`): an overrun then OOM-kills
the worker, which the operator restarts, where a swapping node pages kubelet
out and needs a power cycle; and use an image that pre-warms its JIT kernels,
because compiling a cutlass GEMM on the serving node costs about 6 GB of host
RAM per nvcc process on top of 79.5 GiB of pinned weights.

## Limits in this release

- vLLM only. llama.cpp RPC members are the next runtime; the manual pattern
  is in the two-Spark runbook until then.
- One group per service; no autoscaling; recreate is the only update strategy.
- Members take an existing claim; the operator does not stage weights per
  member yet.
- The stock DeepSeek-V4-Flash-Vision image needs a FlashInfer prefill patch
  on GB10 until it ships upstream; the LLMKube runtimes image carries it.

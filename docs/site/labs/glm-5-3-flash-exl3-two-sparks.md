---
title: GLM-5.3-Flash EXL3 on two DGX Sparks
description: A 330B hybrid MoE served across two GB10 boxes as one InferenceService, built from a runtime we had to assemble ourselves after the upstream recipe relicensed mid-build. Records the licence boundary, the RoCE fabric config that actually works, the measured decode and prefill numbers, and the five failures between a green build and a served token.
---

# GLM-5.3-Flash EXL3 on two DGX Sparks

This build serves GLM-5.3-Flash across two NVIDIA DGX Spark boxes as a single
`InferenceService` using `spec.multiNode`. It is the lab's local coding
endpoint: an opencode plan, build and review workflow runs against it, and so
does the [Meitheal](https://github.com/defilantech/meitheal) agent that opens
pull requests.

The model is a 330B hybrid MoE, 45 layers of KDA plus DeepSeek sparse
attention, 288 experts with 8 active, one MTP layer and a vision tower. At FP8
it is 328 GB against 121.6 GiB of unified memory per Spark, so a single box is
not close. The quantisation here is EXL3/TR3 at 4 bits per weight: 164 GiB
across 120 shards.

## Why this combination

The interesting part is not that a large model fits across two machines. The
DeepSeek build already showed that. What this one records is what happens when
the recipe you are building on changes its licence in the middle of the work,
and what it costs to rebuild the runtime yourself rather than pull someone
else's image.

The short version: it cost a week of care and nothing measurable in throughput.

## The licence boundary

The community recipe this build follows relicensed from MIT to AGPL-3.0 on
2026-09-07 at 13:43 UTC. Our runtimes repository is Apache-2.0. MIT vendors
into Apache-2.0 with attribution; AGPL-3.0 does not. That also rules out the
upstream prebuilt container image, because those binaries are built from
post-relicense source.

The fix was a timestamp rather than a rewrite. Their own relicense notice
preserves MIT for contributions made before that moment, and a granted licence
cannot be revoked, so the last MIT commit is a usable base indefinitely.

The detail that mattered: their largest performance contribution, a grouped
mixture-of-experts CUDA kernel worth 37 to 45 percent on cold prefill, was
committed between 04:44 and 06:15 UTC that same morning. Hours before the
relicense. Checking the date alone would have thrown it away. Checking the
timestamp kept it.

Staying on the MIT side costs exactly two overlay patches, both opt-in and off
by default upstream, so the default serving configuration is complete.

Three separate licences are in play and they do not travel together:

| Artifact | Licence | Constraint |
|---|---|---|
| Runtime overlay | MIT | attribution |
| EXL3 checkpoint | ShapleyMcg 1.0 | source-available, commercial use permitted, attribution is a condition |
| DFlash2 drafter | CC BY-NC-ND 4.0 | NonCommercial, NoDerivatives, research and eval only |

The drafter is the binding one. It supplies most of the decode throughput and
its terms are narrower than everything else here, so the configuration measured
below is not one you can run commercially as-is. The licence-safe fallback is
the checkpoint's own MTP head, at a real throughput cost.

## Why a custom runtime at all

Stock vLLM cannot serve this checkpoint. It dies on the first forward with
`pe_dim must be 64 for fp8_ds_mla`. GLM-5.3-Flash is NoPE MLA
(`qk_rope_head_dim=0`, `kv_lora_rank=512`) and the only sparse-MLA backend on
SM12x expects a 576-wide GLM_NSA record. The overlay zero-pads the 512-d latent
into that geometry and registers a real EXL3 quantisation method so routed
experts stay packed as trellis plus suh plus svh plus mcg. Registering the name
`exl3` is not enough; the method has to run the kernels.

The image is built from the Apache-2.0 `vllm/vllm-openai` base pinned by
digest, plus MIT ExLlamaV3 compiled for `sm_121`. It lives in
[llmkube-runtimes](https://github.com/defilantech/llmkube-runtimes) as
`cuda-gb10-vllm-glm53-exl3`.

Build economics, measured rather than estimated: the whole CI job is about 13
minutes on a hosted arm64 runner, of which the CUDA compile is about 6 at
`MAX_JOBS=4`. Upstream fixes `MAX_JOBS` at 8 for a workstation; a hosted runner
has 4 vCPU and 16 GB, where 8 concurrent nvcc processes are OOM-killed and
surface only as a bare `Killed`.

## Hardware

Two DGX Sparks, GB10, `sm_121` aarch64 Grace, 121.6 GiB unified memory each.
A GB10 is not a small B200: no MIG, RDMA rather than NVLink, and 99 KB of
shared memory per block, which matters below.

### Fabric

The members talk over a direct CX7 link on a `/30`. The configuration that
works, and the one that does not, are worth stating separately because they
look equally plausible:

```yaml
multiNode:
  rdmaResource: rdma/rdma_shared_device_a
  ibGIDIndex: 3
  members:
    - node: ahazidgx2
      fabric:
        address: 10.10.2.1
        socketInterface: enp1s0f0np0
        ibHCA: rocep1s0f0
      modelCache: {claimName: glm53exl3-dgx2}
    - node: ahazidgx3
      fabric:
        address: 10.10.2.2
        socketInterface: enp1s0f1np1
        ibHCA: rocep1s0f1
      modelCache: {claimName: glm53exl3-dgx3}
```

`rdmaResource` is the field that makes the rest work. Without it the pod
requests only `nvidia.com/gpu`, never sees `/dev/infiniband`, and NCCL reports
`NET/IB : No device found` before failing with `invalid usage`. The operator
adds the RDMA request, limit and matching securityContext only when that field
is set.

`ibGIDIndex: 3` was verified on the hardware rather than inherited. Index 3
carries `::ffff:10.10.2.1` on the head's device and `::ffff:10.10.2.2` on the
worker's, each matching its own rank. An all-zero GID entry passes every
earlier check and then kills that rank about 60 seconds in with
`ibv_modify_qp` errno 61.

With that in place, TP=2 forms over 50 NCCL channels via `NET/IB`.

## Weights

164 GiB per node, staged from the lab's MinIO rather than pulled from Hugging
Face twice. The launcher's default is to download to the head and rsync to the
worker; pulling both nodes from MinIO in parallel instead measured 43 and 68
MiB/s, about 111 MiB/s combined, which is a 1 GbE uplink saturated and shared.
Around 45 minutes.

Faster next time: stage one node at full line rate, then push to the second
over the CX7 fabric at 109 Gb/s. Roughly halves the wall clock.

The bytes are bound rather than copied. Static local volumes point at each
node's existing cache, because letting the operator download into a fresh
volume would need another 176 GB per node and push both past the 85% mark
where kubelet starts deleting images.

## Context, and why it is 500k rather than 850k

The upstream default is 850k. vLLM refused to start:

```
To serve at least one request with the model's max seq len (850000),
13.46 GiB KV cache is needed, which is larger than the available KV
cache memory (11.75 GiB). Based on the available memory, the estimated
maximum model length is 609280.
```

That 13.46 figure matches upstream's own arithmetic, roughly 7.4 GiB fixed plus
7.1 GiB per million tokens, so the model behaved exactly as documented. We
simply have about 2 GiB less headroom, which fits the difference in how it
runs: their `docker run` sets no memory cgroup, this pod is capped at 110Gi.

500k is upstream's own "boots reliably" figure and what their E3 recipe uses.
Raising `gpu_memory_utilization` to buy the KV back was rejected: on this
unified-memory architecture each 0.01 is about 1.2 GiB taken from host headroom
that long prefills need, and upstream recorded a 256k prefill at 0.87 crashing
the head. Reducing context is the safe direction, and changing one number keeps
the outcome attributable.

500k does not bind the real workload. The agent context window is capped at
220k.

## Measured results

Startup, per node:

| | |
|---|---|
| Weights and non-torch | 85.6 GiB |
| Peak activation | 6.17 GiB |
| CUDA graphs | 0.49 GiB |
| KV cache | 11.62 GiB |
| KV pool | 508,064 tokens, 1.02x at 500k |
| Weight load | about 5 minutes, 120 shards |
| Profile, KV, warmup | 98.5 s |

Decode, measured with upstream's own `bench_decode.py` changing only the
endpoint, so the protocol matches the published numbers: temperature 0,
thinking off, median of 5 runs of 400 tokens, DFlash2 k=7, draft TP=2.

| Measure | This build | Upstream published |
|---|---:|---:|
| Structured decode, 1 stream | 66.8 tok/s | 62.9 and 65.1 |
| Structured accept ratio | 0.953 | 0.959 |
| Accepted per step | 6.67 of 7 | 6.71 |
| Prose decode, 1 stream | 25.7 tok/s | 27.1 |
| TTFT, structured | 0.34 s | 0.72 s |

The accept ratio is what makes the decode number trustworthy. At 0.953 with
6.67 of 7 draft tokens landing per step, speculative decoding is genuinely
working rather than degrading into something that merely looks fast.

Structured came in slightly ahead of upstream and prose slightly behind, but
structured ranged 59.7 to 66.9 across runs and prose 23.4 to 30.0, so the prose
gap is smaller than its own variance. The right comparison for prose is
upstream's stock 27.1, not the 32.1 they publish with two opt-in patches that
are AGPL and deliberately absent here.

Cold prefill, unique salted prompts so every rung is genuinely uncached:

| Prompt | Prefill tok/s |
|---|---:|
| 12k | 1,629 |
| 16k | 1,574 |
| 100k | 1,430 |

Two operational facts from the same run. The first request after boot ran at
397 tok/s against roughly 1,600 warm, a one-time allocation cost that upstream
sees too. Any benchmark that does not discard the first request reports a
number 4x low. And prefix caching works and is block-aligned: an 8k follow-up
hit 7,168 cached tokens of 8,004, exactly the batched-token setting, for 8,713
tok/s.

### What was not measured

Long-context prefill above 100k is not reported here. Single runs at 256k and
300k came in below the published figures, but the 300k rung was faster than the
256k one, and prefill throughput should not improve with length. That inversion
means noise, so the numbers are not sound enough to publish.

Concurrency is also not reported. A sweep was run across chat, coding and
agentic patterns at 1, 2, 4 and 8 streams, but the harness measures for a fixed
60-second window that was tuned for a much faster model. At roughly 25 to 30
tok/s with 1024-token generations, some cells collected two samples, which
cannot support the percentile latencies the harness is designed to produce.

Single-stream is what this build is tuned for and what was measured properly.
The decode batch is set to 4, so anything past that measures queueing rather
than parallelism.

## The workflow this serves

The ring is the local coding endpoint for two things.

**opencode**, with a plan, build and review split. The plan agent reasons and
proposes but cannot edit. The build agent executes the plan and runs its
falsification steps. The review agent runs as a separate primary session with
no access to the plan, because a reviewer that inherits the author's framing
finds nothing. All three run on this ring.

**Meitheal**, which takes a GitHub issue, runs a coder agent against the
repository, gates the result, has a reviewer agent inspect it, and opens a pull
request. The coder runs on this ring; the gate and the reviewer run elsewhere.

Two runs are worth recording together, because the difference between them is
not the model:

| Issue | Difficulty | Rails | Coder time | First write |
|---|---|---|---:|---|
| multiNode dead-member detection | hard, two coupled defects, design left open | withheld | 3 h 56 m | about 2 h |
| Alert label wrong in a Helm template | small, one file | write-first, named neighbour to mirror | 14 m | immediate |

Both produced correct, tested work that passed the gate, drew a GO from the
reviewer, and became a pull request. The 17x difference came from task
difficulty and from whether the harness told the model to write before reading.
Withholding the write-first rail was deliberate, to see what it costs. It costs
two hours of reading.

Both changes were independently verified before their pull requests opened, by
reverting the production change and confirming each new test actually failed.
A test that stays green with its feature removed is not coverage, and a GO
verdict is not evidence on its own.

## What went wrong

Five failures sat between a green build and a served token. Four of them looked
identical from outside: pods being recreated in a loop. That is the operator
correctly recreating a group whose member died, so the visible symptom pointed
away from the cause every time.

**The missing RDMA field.** Covered above. The pod never saw
`/dev/infiniband`. It was missed because an early inspection truncated the spec
at 200 characters and showed only the fields that were then copied. Reading a
spec through a truncating formatter is how a required field becomes invisible.

**The wrong reference config, described as proven.** The fabric was first
modelled on a retired ring that was Stopped and had never been observed
serving, using the LAN address and naming all four host channel adapters. The
config that works uses the `/30` fabric address with a single matching adapter.
"There is a config in the cluster that looks like mine" is not evidence.

**A chat template that contradicted itself.** Requests asking for thinking to
be off were still told to reason at maximum effort, while the generation prompt
closed the thinking block. The model had nowhere to put reasoning and put it in
the answer. This matters for measurement as much as output: every published
decode number for this model is quoted with thinking off, so benchmarking
against the unfixed template compares two different workloads.

**A patch that was in the image and never ran.** The upstream launcher applies
one patch at container start rather than at build. Porting the build steps and
then rewriting the entrypoint left it with no caller. The image built clean,
passed the full gate, had its provenance attested, and then died on hardware
after a five-minute load inside memory profiling, because a kernel that patch
disables needs 128 KB of shared memory per block and GB10 has 99 KB.

That one is the most useful. The build guards asserted that overlay files were
present and matched vendored fixtures. Both were true while the image was
broken. **Presence is not effect.** Worse, the first design had a fail-closed
entrypoint written specifically against silent patch skips; it was removed for
a correct reason, a Kubernetes container command overrides an image entrypoint,
and replaced with checks that could not catch that class of fault. The
replacements now assert the outcome: that the patched condition literally reads
what it should, both at build and against the shipped image.

**The context default that did not fit.** Covered above, and the only one of
the five that failed loudly with the answer in the error message.

## Reproducing this

The runtime image, its Dockerfile and its Tier-1 gate are in
[llmkube-runtimes](https://github.com/defilantech/llmkube-runtimes) under
`cuda-gb10-vllm-glm53-exl3`. The Dockerfile header records the licence boundary
and the exact upstream commit it derives from; do not update the vendored
overlay from upstream main, because anything after 2026-09-07 13:43 UTC is
AGPL-3.0 and CI enforces that on both the source tree and the shipped image.

The weights and the drafter carry their own terms, set out above. Read them
before serving this anywhere that matters.

## Related

- [DeepSeek V4 Flash Vision on two DGX Sparks](/docs/labs/deepseek-v4-flash-two-sparks)

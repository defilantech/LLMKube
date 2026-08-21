# Serving one model across two DGX Sparks (llama.cpp RPC)

One model, two GB10s, their ConnectX-7 link between them. The memory is not
pooled by hardware: the runtime splits the model, with `ggml-rpc-server`
exposing the remote Spark's devices and the main `llama-server` offloading
layers onto them over the fabric. Capacity becomes the SUM of both machines'
memory (~238GiB usable) instead of either one's (~119GiB).

This is the pattern for any model in the 120–235GiB band at serve time, and
it is the two-node rehearsal for paired DGX-class hardware generally. It was
proven with DeepSeek V4-Flash (144GiB MXFP4) and is exercised routinely.
NVIDIA builds their own Spark llama.cpp the same way
(`nvidia/dgx-spark-playbooks`).

The operator does not model multi-node serving yet ([#1423]); this runbook is
the manual pattern #1423 will absorb. Every step below that is by hand is a
requirement on that issue.

[#1423]: https://github.com/defilantech/LLMKube/issues/1423

## Prerequisites

1. **The RoCE fabric is addressed.** From `llmkubelab`:

   ```bash
   ansible-playbook dgx-rdma.yml -K              # configure (idempotent)
   ansible-playbook dgx-rdma.yml -K --tags verify # peer ping + RDMA bandwidth
   ```

   This gives the point-to-point /30s: `10.10.10.1` (ahazidgx1) ↔
   `10.10.10.2` (ahazidgx2) on `rdma0`. Nothing works until the verify play
   passes; the links come up at layer 2 with no addressing.

2. **The runtime image carries the RPC backend.** `cuda-gb10` is built with
   `GGML_RPC=ON` and ships `ggml-rpc-server`
   (llmkube-runtimes #32, image `2a9f998` or later). The build guard asserts
   the binary exists, because upstream leaves RPC off and its absence is
   silent: a runtime without it merely rejects `--rpc` as a bad flag.

3. **The model is in MinIO** under its HF-mirroring key, with the mmproj
   beside the weights if the model is multimodal (`minio-preload.yml`).

## Step 1: the RPC worker on the remote Spark

A plain pod, pinned to the remote node, on hostNetwork so it binds the RoCE
address directly. The RPC protocol is unauthenticated by design; binding the
10.10.10.x address keeps it off every routable interface, which is the
entire security model. Do not bind 0.0.0.0.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: rpc-worker-ahazidgx2
  namespace: default
spec:
  nodeName: ahazidgx2
  hostNetwork: true
  restartPolicy: Always
  containers:
    - name: rpc
      image: ghcr.io/defilantech/llmkube-llama-cuda-gb10:candidate-2a9f99817fda084a9c59b0df6943cd29ad683cc3
      command: ["/app/ggml-rpc-server"]
      args: ["-H", "10.10.10.2", "-p", "50052"]
      resources:
        requests: { memory: "116Gi", cpu: "4", nvidia.com/gpu: 1 }
        limits:   { memory: "118Gi", nvidia.com/gpu: 1 }
```

The memory bounds are not optional. On unified-memory hardware the worker's
tensor allocations are ordinary node RAM; an unbounded worker receiving half
of a very large model rides the node into kubelet eviction, and the failure
arrives on the MAIN side as `send failed ... Remote RPC server crashed`,
which reads as a network fault. A bounded worker dies a clean cgroup OOM
instead, and the node survives to report it.

Confirm it is listening on the fabric address and nothing else:
`ss -tlnp | grep 50052` on ahazidgx2 must show `10.10.10.2:50052`, not `*:50052`.

## Step 2: the InferenceService on the main Spark

A normal single-node InferenceService, pinned to the main node, with two
extras: `--rpc` pointing at the worker's fabric address, and every GPU layer
offloaded. Only the main pod reads the GGUF; tensors for the offloaded
layers stream to the worker once, at load.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: ornith-397b-2spark
spec:
  modelRef: ornith-397b
  image: ghcr.io/defilantech/llmkube-llama-cuda-gb10:candidate-2a9f99817fda084a9c59b0df6943cd29ad683cc3
  nodeSelector:
    kubernetes.io/hostname: ahazidgx1
  resources:
    cpu: "8"
    gpu: 1
    memory: "118Gi"
  extraArgs:
    - "--rpc"
    - "10.10.10.2:50052"
    - "--no-mmap"
    - "--ctx-size"
    - "8192"
```

`spec.resources.gpu` is what makes the operator add the GPU-taint
toleration; omit it and the pod sits Pending with a misleading affinity
message. `--no-mmap` is mandatory at this scale on unified memory: mmap
pages the whole artifact through the page cache ON TOP of the node's UMA
share of the weights, which is a 2x memory demand on the main node. The
same lesson as the Strix >64GB rule, at twice the size. Keep the first
bring-up's context small; grow it only after the fit is proven.

The corresponding Model stages from MinIO; multi-artifact via `spec.files`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: ornith-397b
spec:
  source: s3://models/ornith-ai/Ornith-1.5-397B-GGUF
  files:
    - Ornith-1.5-397B-Q4_K_M.gguf
    - mmproj-Ornith-1.5-397B-BF16.gguf
  sourceSecretRef: { name: minio-models }
```

The model cache PVC and the download land on the MAIN node (RWO local-path
pins them there, the same way the DeepSeek caches pinned to ahazidgx1).
The main node therefore needs disk for the full artifact even though its
GPU holds only part of it.

## Step 3: verify at the wire, not at Ready

`status.phase: Ready` proves the server started; it does not prove the model
spans two machines. Three checks, in order:

1. **Both GPUs are loaded.** `nvidia-smi` on EACH Spark shows a large
   resident allocation. One busy GPU and one idle one means `--rpc` was
   dropped or rejected: check the serving pod's logs for the flag echo.
2. **Traffic crosses the fabric during decode.** On either node:
   `sar -n DEV 1 5` (or `ip -s link show enp1s0f1np1` twice) while a
   completion streams. Per-token activations traverse the link; a silent
   fabric during generation means the split is not real.
3. **A completion is correct.** The usual curl against the OpenAI endpoint,
   with a prompt whose answer you know.

Only after all three does the deployment count as working. A wire-silent
"Ready" has fooled this fleet before.

## Sizing

Weights must fit the sum minus overheads: ~238GiB usable across two Sparks.
Ornith-1.5-397B Q4_K_M (224GiB) fits with ~14GiB for KV, compute buffers,
and the vision tower; its hybrid linear attention keeps KV at ~30KB/token,
so context length is not the constraint it usually is. Q5_K_M (262GiB) does
not fit. For dense models, remember decode is bandwidth-bound and RPC adds
per-layer round-trips: expect single-node-class throughput only from MoE
models with small active sets.

## Known limits

- The RPC backend is upstream-labelled proof-of-concept: no auth, no TLS.
  The fabric-address binding is the security boundary. Never expose it on a
  routable interface.
- One worker per additional node; scale-out beyond two Sparks is untested
  here (NVIDIA's playbooks cover 3+ via a switch).
- New architectures need a runtime image whose llama.cpp knows them: a
  day-zero model (e.g. `qwen35moe`) requires a `cuda-gb10` rebuild before
  any of this applies.
- Budget with the REAL denominator: k8s allocatable minus system overhead
  is ~118GiB per Spark, not 128. A 224GiB artifact leaves ~4-6GiB of margin
  per node with nothing to spare; the first bring-up attempt without these
  bounds took a node down hard enough to kill ssh. If a quant near 190-200GiB
  exists, prefer it for the first proof and step up after.
- When one side dies mid-transfer, the surviving rpc-server can block in an
  uninterruptible send to its dead peer: the pod hangs in Terminating until
  TCP gives up. Expect worker restarts to be slow after a peer crash.
- Operator gaps this runbook papers over, tracked in [#1423]: the worker pod
  is hand-placed and unmonitored, the pair has no shared health, and a
  worker restart mid-serve is undetected until requests fail.

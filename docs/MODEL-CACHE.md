# Persistent Model Cache

LLMKube includes a persistent model cache that avoids re-downloading models when InferenceServices are deleted and recreated.

## Overview

Without persistent caching, models are downloaded via init container every time a pod starts. For large models (13B-70B), this means 26-40GB+ downloads taking 10-30+ minutes each time you recreate a deployment.

With persistent caching:
- A PVC is created **automatically** in each namespace where you deploy models
- Models are downloaded **once** to the namespace's PVC
- Subsequent pods mount the cache and skip download
- Delete/recreate cycles complete in seconds

## Architecture

LLMKube uses **per-namespace PVCs** for model caching. This provides:
- **Namespace isolation**: Each namespace has its own cache
- **No cross-namespace dependencies**: Models work independently
- **Simple RBAC**: No need for cross-namespace access

```
┌─────────────────────────────────────────────────────────────┐
│                 Namespace: production                        │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          llmkube-model-cache PVC                     │    │
│  │  /models/<cache-key>/model.gguf                     │    │
│  └─────────────────────────────────────────────────────┘    │
│           ▲                              ▲                   │
│           │ (init container writes)      │ (read-only)       │
│           │                              │                   │
│  ┌────────┴────────┐        ┌────────────┴──────────────┐   │
│  │  First Pod      │        │  Subsequent Pods          │   │
│  │  - Downloads    │        │  - Mount cache read-only  │   │
│  │  - Caches model │        │  - Skip download          │   │
│  └─────────────────┘        └───────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                 Namespace: staging                           │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          llmkube-model-cache PVC                     │    │
│  │  /models/<cache-key>/model.gguf                     │    │
│  └─────────────────────────────────────────────────────┘    │
│                          ▲                                   │
│                          │                                   │
│                 ┌────────┴────────┐                         │
│                 │  Pods in staging │                         │
│                 └─────────────────┘                         │
└─────────────────────────────────────────────────────────────┘
```

## Deploying to Any Namespace

You can deploy models to any namespace using the CLI:

```bash
# Deploy to production namespace
llmkube deploy llama-3.1-8b --gpu -n production

# Deploy to staging namespace
llmkube deploy llama-3.1-8b --gpu -n staging

# Deploy to default namespace
llmkube deploy llama-3.1-8b --gpu
```

The controller will automatically:
1. Create a `llmkube-model-cache` PVC in the target namespace (if it doesn't exist)
2. Configure the pod's init-container to download the model to the PVC
3. Mount the PVC read-only for the main container

**Note**: Each namespace has its own PVC, so the same model deployed to multiple namespaces will be downloaded once per namespace.

## Cache Key

Models are cached using a SHA256 hash of the source URL (first 16 characters). This means:
- Models with the same source URL share cache entries
- Changing the source URL creates a new cache entry
- The cache key is stored in `Model.Status.CacheKey`

Example:
```
Source: https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf
Cache Key: a3b8c9d4e5f67890
Path: /models/a3b8c9d4e5f67890/model.gguf
```

## Integrity Verification (spec.sha256)

Setting `spec.sha256` on a `Model` makes the download init container verify the
artifact's bytes against the expected digest before anything becomes the cache.
A mismatch fails the init container, so the pod never starts the inference
container with bad weights.

- The transfer lands in a partial file and is renamed onto the final path only
  after the hash matches. On a mismatch the partial is discarded and the start
  fails, so bad bytes never become the cache.
- A rejected publish leaves a `<model>.<sha256>.sha256-rejected` marker. The
  next start fails before any transfer, so a wrong pin cannot re-download a
  multi-gigabyte artifact on every kubelet backoff. The marker is keyed on the
  expected digest: correcting `spec.sha256` makes it inert, and another Model
  sharing the cache directory cannot clear it. Delete it to force a retry.
  This mirrors the metal-agent's `digestMismatchMemo`, without its in-process
  lifetime.
- On `RefreshPolicy: OnChange`, an upstream that has moved past the pin keeps
  the pinned cached copy and exits 0; the marker then makes later starts skip
  the re-fetch, so a moved upstream does not re-download on every rollout. The
  log names the digest mismatch rather than blaming reachability.
- A verified artifact gets a `<model>.sha256` stamp recording the digest, the
  file size and its modification time, so a stamp can only vouch for the
  exact bytes that were hashed. A later start skips re-hashing gigabytes only
  while all three still agree; a missing, stale or differently-sized file is
  hashed once and re-stamped. The stamp is written to a temp file and renamed,
  so a concurrent reader never sees a truncated one. The metal agent's model
  store writes and reads the same triple, so a stamp from either side is
  legible to the other, and a bare-digest stamp from an older agent release is
  hashed once and rewritten.
- A cached file that fails the re-hash was corrupted outside any download. It
  is left in place (the cache directory is keyed on the source alone and may
  be shared with another Model) and the same start replaces it with a fresh
  verified download.
- The gates fail closed: a download built for a digest-pinned Model aborts if
  the digest does not reach the container instead of transferring unchecked.
- Verification also runs on prefetch Jobs, which reuse the same init
  container.
- A pre-staged `pvc://` source with `spec.sha256` verifies the mounted file in
  an init container at pod start. The mount is read-only, so no stamp is
  written: the file is hashed on every pod start. A mismatch fails the pod.
  The hash is added to every pod start, rollout, reschedule and scale-out:
  expect roughly (file size / hash throughput), which for a 100-400 GB
  artifact on a network PVC can be many minutes. `oci://` sources stay
  digest-addressed and are rejected with `spec.sha256`.
- Combining `spec.sha256` with multi-file staging (`spec.files` / `spec.mmproj`)
  or an `oci://` source is rejected at admission; use `spec.fileSha256` for a
  multi-file set. This is a behavior change for a Model that set such a
  combination before this release: the digest was accepted and silently
  ignored then. Remove the ignored `spec.sha256` (or the multi-file / `oci://`
  source) to keep the Model admissible.

### Multi-file staging (`spec.fileSha256`)

`spec.sha256` attests to a single artifact, so it cannot pin a multi-file
staging set. Use `spec.fileSha256`, a map from repo-relative path to digest:

```yaml
spec:
  source: hf://org/repo-GGUF
  files:
    - Model-00001-of-00002.gguf
    - Model-00002-of-00002.gguf
  fileSha256:
    Model-00001-of-00002.gguf: 9f2c...   # 64 hex characters
    Model-00002-of-00002.gguf: 1ab3...
```

- Only listed files are verified; a file with no entry is staged as before, so
  a partially pinned set is allowed. An unpinned shard in a partially pinned
  set gets no protection, so pinning the whole set is recommended.
- `spec.files` is capped at 256 entries of 512 characters each.
- Each pinned file is hashed before it is renamed onto the cache. A mismatch
  discards that file's partial and records its own
  `<file>.<sha256>.sha256-rejected` marker, and the loop stops. Once the
  upload is fixed, delete the marker (`<file>.<sha256>.sha256-rejected`) to
  let the next start re-download.
- The per-file stamp and fail-closed behavior match the single-file path.
- Every key must name a `spec.files` entry or `spec.mmproj`. Globs in
  `spec.files` are rejected when `fileSha256` is set, because a digest cannot
  be keyed to an unknown expanded name. `spec.sha256` and `spec.fileSha256`
  are mutually exclusive, and `fileSha256` is rejected on an `oci://` source.

## Prefetch (Eager Download)

By default a `Model` with a remote source is only a declaration: nothing is
downloaded until the first `InferenceService` referencing it starts, and the
download then runs in that pod's init container, on the serving node. Set
`spec.prefetch: true` to have the operator stage the artifact into a model
cache claim ahead of the serving pods:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-3b-prefetch
spec:
  source: hf://bartowski/Llama-3.2-3B-Instruct-GGUF/Llama-3.2-3B-Instruct-Q4_K_M.gguf
  format: gguf
  prefetch: true
```

The controller runs an owner-referenced download Job that reuses the same
init-container downloader the serving path uses. `status.phase` moves
`Downloading` -> `Ready`; once `Ready`, the first `InferenceService` starts
from a cache hit instead of a cold download. A glob in `spec.files` is
expanded from the repository listing before the Job is built, exactly as it
is for the serving pods, so a multi-file checkpoint such as
`files: [config.json, "*.json", "*.safetensors"]` prefetches too.

### Where the download lands

The Job stages into one claim, chosen in this order:

1. `spec.modelCache.claimName` on the Model, when set. The Job writes there
   and never falls back, whatever services exist.
2. Otherwise the single `spec.modelCache.claimName` the referencing
   `InferenceService`s agree on (#1676).
3. Otherwise the namespace's shared cache (`llmkube-model-cache`, created on
   demand).

`spec.modelCache` on the Model takes the same fields as an
`InferenceService`'s: `claimName`, `persistence`, `storageClassName` and
`prefetch` (an alias of `spec.prefetch`). It is also the default for the
services that reference the Model: a service with no `modelCache` block of
its own resolves the Model's, so the claim is declared once. A service that
sets its own block still wins. `persistence: Ephemeral` cannot be combined
with prefetch; the API server rejects it.

Every claim the Model has been staged into is recorded in `status.prefetch`,
one entry per claim with the Job name, the phase (`Pending`, `Running`,
`Succeeded`, `Failed`) and a fingerprint of what was staged. A spec change
that alters the files (the source, `files`, a revision, `fileSha256`) runs
a new prefetch; one that does not (placement, tolerations) only refreshes
the entry. Deleting a finished Job retries it.

Two conditions report what the field could not do:

- `PrefetchSkipped=True` (reason `SourceNotPrefetchable`): `spec.prefetch`
  is set on a source the downloader does not stage (local paths, `pvc://`,
  `oci://`, `s3://`).
- `PrefetchTargetMismatch=True` (reason `ClaimMismatch`): a referencing
  service mounts a claim the Model was not staged into, so that service
  downloads at serve time. Once a download has completed the operator does
  not move it: a service that appears later with another claim is reported
  here, and gets a `PrefetchTargetMismatch` warning event of its own. Set
  `modelCache.prefetch` on that service, or `modelCache.claimName` on the
  Model, to stage there. While the Job is still running, a service that
  appears with another claim retargets it instead.

### Letting the service require a prefetch

An `InferenceService` can ask for its own staging with
`spec.modelCache.prefetch: true`, whether or not the Model sets
`spec.prefetch`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen2-5-0-5b-instruct
spec:
  source: Qwen/Qwen2.5-0.5B-Instruct   # about 1 GiB; a gated repo adds sourceSecretRef
  format: safetensors
  files: [config.json, "*.json", "*.safetensors"]
  modelCache:
    claimName: model-cache
  prefetchAffinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - {key: nvidia.com/gpu.present, operator: DoesNotExist}
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen
spec:
  modelRef: qwen2-5-0-5b-instruct
  runtime: vllm
  modelCache:
    claimName: model-cache
    prefetch: true
```

With the field set:

- The Model controller runs a prefetch Job for this Model into the claim
  this service's pods mount (`claimName`, else the Model's
  `modelCache.claimName`, else the operator cache), placed by the Model's
  `prefetchNodeSelector`, `prefetchTolerations` and `prefetchAffinity`. The
  download happens on the nodes meant for downloads, never on the serving
  nodes. One Model can be staged into several claims; each has its own Job
  (`<model>-prefetch-<claim hash>`) and `status.prefetch` entry.
- The service creates no pods until that prefetch has succeeded for the
  Model's current spec, and reports `ModelPrefetched=False` with reason
  `WaitingForPrefetch` (also in `status.schedulingStatus`) meanwhile, or
  `WaitingForModel` while the Model has not derived its cache key yet. A
  later change that needs new weights holds the same way: the live
  Deployment keeps serving the old template until the new prefetch
  completes, then rolls. A failed prefetch shows as `PrefetchFailed` and the
  workload stays as it was; delete the Job to retry. While a hold is on, a
  replica change other than to zero waits with the template, and a
  `multiNode` group that loses a member is recreated only once the
  prefetch finishes.
- Once staged, the serving pods only consume the cache: the claim is
  mounted read-only, there is no `model-cache-prep` or `model-downloader`
  init container, and a `model-verify` init container checks the files the
  Job recorded on the entry are present (and hashes a single file against
  `spec.sha256` when one is pinned). If a file is missing the pod fails with
  `PrefetchedFilesMissing` (surfaced on the `ModelTransferHealthy`
  condition) rather than downloading on the serving node. The prefetch sets
  no Hub-offline variable: set `spec.hubOffline: true` when the runtime
  must not reach the Hub for anything the Job did not stage (see the
  air-gapped guide).
- Use a `ReadWriteMany` (or `ReadOnlyMany`) claim when the serving pods run
  on other nodes than the Job. A `ReadWriteOnce` claim attaches to one node:
  the first staging works because the Job finishes before the pods attach,
  but a later re-stage or re-verification while pods mount the claim
  elsewhere stalls on `Multi-Attach` and is reported as `PrefetchStalled`.
  On a single-writer claim the Model therefore skips the routine
  re-verification while a pod mounts the claim (the entry says so and
  stays usable), and a re-stage after `PrefetchedFilesMissing` still runs
  but needs the consuming service suspended (`spec.suspend`) to release
  the attachment when the Job lands on another node.
- `prefetch: true` means "serve only from a prefetched cache". A request the
  pods could not consume is refused, not ignored: the service reports
  `ModelPrefetched=False` with reason `PrefetchUnsupported`, creates no pods
  (or keeps its live Deployment as it is), and no Job runs. That covers
  model caching disabled on the operator, a runtime the operator does not
  stage (TGI, `generic` without `stageModel`, `skipModelInit: true`), a
  `speculativeDecoding.draftModelRef` (the draft is not prefetched), a
  `multiNode` member with its own `modelCache.claimName` (that member's pod
  mounts a claim the prefetch does not fill), and a source the downloader
  does not stage ahead of time (`s3://`). The field
  is `PrefetchNotApplicable`, and not waited for, only where nothing
  downloads at serve time: Metal, `pvc://`, `oci://` and local sources.

Applied together by GitOps, the pair above is safe in either order: the
Model names the claim, the service inherits or repeats it, and the service
waits for the staging it asked for. The request itself is not inherited:
`modelCache.prefetch` on the Model is `spec.prefetch`, which stages the
Model's own claim and holds no service, so a service that must wait sets
`modelCache.prefetch` in its own block.

### Placing the prefetch pod

A node-pinned cache (a `WaitForFirstConsumer` local-path claim) binds to
whichever node the prefetch pod schedules to. `spec.prefetchTolerations`,
`spec.prefetchNodeSelector` and `spec.prefetchAffinity` steer that placement
so the model lands on the node that will serve it, or stays off the GPU
nodes while it downloads:

```yaml
spec:
  prefetch: true
  prefetchTolerations:
    - key: example-gpu
      operator: Exists
  prefetchNodeSelector:
    example.com/node-pool: gpu
  prefetchAffinity:
    nodeAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          preference:
            matchExpressions:
              - {key: example.com/node-pool, operator: In, values: [downloads]}
```

All three govern only where the download runs, never where the model is
served.

Placement has to reach the claim. A claim on one node's local disk (a
`WaitForFirstConsumer` local-path volume that is already bound, a hostPath
PV) can only be filled from that node, so a rule that keeps the Job off the
GPU nodes leaves it unschedulable when the claim lives on one. The Job pod
requests no GPU, so pinning it to the claim's node with
`prefetchNodeSelector` still keeps the download out of the serving pods; an
off-GPU rule is for a claim every node can reach (an RWX class, a blob
mount). A Job whose pod cannot be scheduled never completes and never fails:
the Model stays `Downloading`, its `status.prefetch` entry and `Progressing`
condition carry the scheduler's message (reason `PrefetchUnschedulable`, for
example `volume node affinity conflict`), and a `PrefetchUnschedulable`
warning event is recorded. A pod that was scheduled but has not started
within five minutes (an image pull, a volume that cannot attach) is reported
as `PrefetchStalled`, with the waiting reason of the container that blocks.
A downloader init container that is running is not a stall: Kubernetes keeps
a pod with init containers in phase `Pending` until all of them finish, so a
download in progress reads as downloading. Fix the placement or the claim,
then delete the Job to retry.

Limitations:

- Prefetch applies to remote sources (`https://`, `hf://`, and a bare
  repository ID with `spec.files`). Local paths, `pvc://`, `oci://` and
  `s3://` sources set `PrefetchSkipped` instead.
- In `perService` cache mode the operator-created `<isvc>-model-cache`
  claim binds where the first consumer schedules. A Model-level
  `spec.prefetch` targets the shared cache there and is reported as a
  `PrefetchTargetMismatch`; `modelCache.prefetch` on the service stages into
  the per-service claim, with the Job as that first consumer, so set the
  Model's prefetch placement to the node the service will run on.
- A failed prefetch Job sets `status.phase: Failed` on a Model that asked
  for it; check the Job's pod logs (the name is in `status.prefetch`). The
  Job self-cleans 24 hours after finishing and is garbage-collected with
  the Model.
- There is no timeout on a prefetch Job. A pod that cannot be scheduled is
  reported as above; a pod that is scheduled but stalls (a slow or hung
  source) is visible only in its logs.

### When the staged files disappear

`status.prefetch` records that a claim was filled; it cannot see the claim
afterwards. If the files go away (`llmkube cache clear`, a claim deleted and
recreated under the same name, a wiped volume), two things bring them back
without a rollout:

- The serving pods of a service with `modelCache.prefetch` fail
  `model-verify` with `PrefetchedFilesMissing`, which the service reports on
  `ModelTransferHealthy`. The service then deletes the Model's prefetch Job
  for its claim, the Model stages again, and the failing pods pass
  `model-verify` on their next init retry. This happens once per failure
  episode: a claim the serving node cannot reach at all stays reported
  rather than re-staged in a loop.
- The Model re-verifies a claim whenever its prefetch Job is gone: a day
  after it finished (the Job's TTL), or when someone deletes it. With the
  files still in place that is a cache hit and completes in seconds. The
  entry stays `Succeeded` and the Model `Ready` while the check runs, so no
  service is held for it; the entry's message and the Model's `Progressing`
  condition (reason `PrefetchReverifying`) show it, and only a failed check
  moves the entry to `Failed`. So "delete the Job" is always an available
  retry, and a wiped claim is refilled within a day even with no pods to
  notice.

See `config/samples/model_prefetch.yaml` and
`config/samples/model_prefetch_gitops.yaml` for complete examples.

## Configuration

### Helm Values

```yaml
modelCache:
  # Enable persistent model cache (default: true)
  enabled: true

  # Provisioning mode: shared or perService.
  # shared (default): a single cluster-wide PVC that all InferenceServices share.
  # perService: operator-managed per-InferenceService PVC that binds on the node
  #   where the inference pod schedules. A separate user-managed override is
  #   available via spec.modelCache.claimName on individual InferenceServices.
  mode: shared

  # Storage size for model cache
  size: 100Gi

  # Storage class (leave empty for default)
  storageClass: ""

  # Access mode
  # - ReadWriteOnce: Single-node clusters
  # - ReadWriteMany: Multi-node clusters (requires NFS, EFS, etc.)
  accessMode: ReadWriteOnce

  # Mount path inside controller pod
  mountPath: /models

  # PVC annotations (e.g., for backup policies)
  annotations: {}
```

### Multi-Node Clusters

For multi-node clusters where pods may run on different nodes, you need a storage class that supports `ReadWriteMany`:

**AWS EKS (EFS):**
```yaml
modelCache:
  storageClass: efs-sc
  accessMode: ReadWriteMany
```

**GKE (Filestore):**
```yaml
modelCache:
  storageClass: filestore-standard
  accessMode: ReadWriteMany
```

**Azure AKS (Azure Files):**
```yaml
modelCache:
  storageClass: azurefile-premium
  accessMode: ReadWriteMany
```

**On-Premise (NFS):**
```yaml
modelCache:
  storageClass: nfs-client
  accessMode: ReadWriteMany
```

The volume must support file locking. Downloaders that share a cache directory take turns through `flock` on `.llmkube-download.lock`, so pods on different nodes rely on the volume to enforce that lock across nodes. NFSv4 and CephFS provide it. An NFSv3 mount with `nolock` does not, so two pods downloading the same model at once can corrupt it. Init images that override the default must provide `flock`.

#### Strictly tainted GPU nodes

When a GPU node carries a hard `NoSchedule` taint, dynamic provisioning can fail for reasons unrelated to the inference pod's tolerations. The safest approach is to pre-provision a PVC bound to the tainted node and reference it through `spec.modelCache.claimName`.

The inference pod already receives the GPU toleration derived from the Model's `hardware.gpu` configuration. The `model-downloader` runs as an init container in that same pod, so a pre-existing PVC does not require a separate download Job.

The claim must already exist and be node-aligned by the cluster administrator's storage setup. `claimName` is user-owned: LLMKube mounts it through the same prep and download init containers as the built-in cache, but never creates, mutates, or deletes it. If the named PVC is missing, the InferenceService is marked Degraded rather than silently falling back to the shared cache.

**Note:** `claimName` is ignored for `pvc://` model sources because those weights are already staged on the cluster and mounted read-only; no download occurs.

Some dynamic provisioners create a per-node helper pod to provision volumes. This helper pod is separate from the LLMKube inference pod and typically carries only the default not-ready/unreachable tolerations. When a GPU node has a hard `NoSchedule` taint, the helper pod may remain Pending with an untolerated-taint event — even though the inference pod itself has the correct GPU toleration.

Strict-taint choices:

- Pre-provision a static, node-aligned claim and use `claimName`.
- Use a provisioner whose helper configuration supports the taint.
- Apply a narrowly scoped cluster-level policy maintained by the cluster administrator.

#### Recognising a volume the provisioner never stamped

The failure above is worth calling out separately because it does not look like a
storage failure. Some provisioners publish a PersistentVolume even when their
helper never ran, carrying a node affinity that no node satisfies:

```console
$ kubectl get pv <pv> -o jsonpath='{.spec.nodeAffinity}'
{"required":{"nodeSelectorTerms":[{"matchExpressions":[
  {"key":"kubernetes.io/hostname","operator":"In","values":[""]}]}]}}
```

An empty value in that `In` term is the fingerprint: node label values are never
empty, so the volume can never be scheduled anywhere. The PVC still reports
`Bound`, which is why this reads as success.

The consuming pod then sits `Pending` forever, and the scheduler describes it in
one of two ways depending on whether the pod had already been assigned a node:

```text
0/4 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity
```

```text
running PreBind plugin "VolumeBinding": binding volumes: pv "pvc-..."
node affinity doesn't match node "gpu-node-1": no matching NodeSelectorTerms
```

Both name node affinity, which sends you to the scheduler. The scheduler is not
at fault: it chose correctly, and the PVC's `volume.kubernetes.io/selected-node`
annotation agrees with it. Provisioning is what failed.

LLMKube reports this directly rather than passing the scheduler's wording
through:

```console
$ kubectl get inferenceservice <name> -o jsonpath='{.status.schedulingStatus}'
UnbindableModelCache

$ kubectl get inferenceservice <name> -o jsonpath='{.status.schedulingMessage}'
model cache volume was never provisioned: PersistentVolume "pvc-..." has a node
affinity that matches no node. Claim: <name>-model-cache. ...
```

To recover, delete the claim so its volume is released, then provision on a
storage class whose helper can run on the tainted node. Which objects you have to
remove depends on the cache mode:

| Cache mode | Deleting the InferenceService clears it |
| --- | --- |
| `perService` | Yes, the claim is owner-ref'd and garbage-collected with it |
| `shared` (default) | No, the shared claim intentionally outlives any one service |
| `claimName` (bring your own) | No, LLMKube never deletes a user-owned claim |

In the latter two cases the unusable volume survives the InferenceService, so
delete the claim explicitly before retrying. Recreating the service against a
claim that is still bound to a broken volume reproduces the same `Pending` state.

#### Choosing a provisioner that tolerates taints

Whether this is fixable by configuration depends on the provisioner:

| Provisioner | Helper tolerations configurable |
| --- | --- |
| `rancher.io/local-path` | Yes, via `helperPod.yaml` in its ConfigMap |
| `microk8s.io/hostpath` | No, the helper template is built into the image |

For `local-path`, add tolerations to the helper template so it can run wherever a
volume is legitimately requested. This mirrors what a CSI node plugin does:

```yaml
# local-path-config ConfigMap, helperPod.yaml key
apiVersion: v1
kind: Pod
metadata:
  name: helper-pod
spec:
  priorityClassName: system-node-critical
  tolerations:
    - operator: Exists
  containers:
    - name: helper-pod
      image: busybox
```

Then point model caches at it:

```yaml
modelCache:
  storageClass: local-path
```

`microk8s.io/hostpath` exposes no equivalent knob, so on a tainted node the
remaining options are the pre-provisioned `claimName` route above or a different
storage class.

### Per-InferenceService Cache PVC (Bring Your Own)

The cache backend above is an operator-global choice. To point a *single*
InferenceService at its own pre-existing, user-owned PVC — for example a
node-local volume for a large model pinned to one node, while everything else
rides the shared cache — set `spec.modelCache.claimName`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3.1-70b
spec:
  modelRef: llama-3.1-70b
  modelCache:
    claimName: my-model-cache   # pre-existing PVC in the same namespace
```

Behavior:

- The named PVC becomes the writable model cache for this workload only: the
  same `model-downloader` init container runs against it, and, under
  `modelCache.prep: always`, the `model-cache-prep` one before it. Weights land
  under the usual `<cacheKey>/` subdirectory, and the serving container mounts
  it read-only. `RefreshPolicy` and cache-key semantics are
  unchanged, so multiple models can safely share one claim.
- The block can live on the Model instead: a service with no `modelCache`
  of its own resolves `Model.spec.modelCache`, so a claim shared by every
  service of a model is declared once (see Prefetch above). A service's own
  block still wins.
- The operator **never creates or deletes** the claim — you own it end-to-end
  (unlike `perService` mode, where the operator provisions and
  garbage-collects `<isvc>-model-cache`). If the claim does not exist, the
  InferenceService is marked `Degraded` with a `ModelCachePVCNotFound` event
  instead of silently falling back to the shared cache.
- `claimName` targets the download path, so it is ignored — with a
  `ModelCacheClaimIgnored` warning event — for pre-staged `pvc://` model
  sources (mounted read-only, no download), and whenever caching is inactive
  (chart `modelCache.enabled: false`, a local `file://` source, or a remote
  model not yet fingerprinted); in the inactive-caching cases the pod falls
  back to an ephemeral emptyDir and re-downloads on every restart.
- `claimName` is mutable: changing it rolls the Deployment onto the new claim,
  but the old claim (and the weights on it) is **not** garbage-collected —
  clean it up yourself if it is no longer needed.
- Node alignment is your responsibility: for an RWO or node-local claim, use
  `nodeSelector` so the pod lands where the PVC binds (a
  `WaitForFirstConsumer` local class binds on the first consumer; a pre-bound
  RWO PVC pins the pod).
- `llmkube cache list` discovers the shared cache and operator-managed
  per-service cache PVCs. A user-managed `spec.modelCache.claimName` PVC is
  outside the operator's cache label/discovery contract and may not appear in
  the listing. Each claim is read through a short-lived read-only pod the
  command creates and deletes, including a Pending `WaitForFirstConsumer`
  claim, which that pod binds. On a multi-node cluster the pod is pinned to
  the node whose kubelet holds the claim, so a strict RWO serving pod does not
  make it stall on Multi-Attach; a claim nothing holds is not pinned, and the
  pod is its first consumer. `cache list` needs permission to list, get,
  create, exec into and delete pods in the namespace, and exits nonzero when
  it cannot read any discovered claim. The inspector copies the holder's
  tolerations, so a tainted node that runs the claim does not reject it.

### Per-Service Storage Class

`spec.modelCache.storageClassName` overrides the operator-global
`--model-cache-storage-class` flag for the cache claim the operator creates,
so a mixed cluster can cache on local disk where the cluster's default class
cannot attach, while the operator still creates and garbage-collects the PVC:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: onprem-llama
spec:
  modelRef: llama-3b
  modelCache:
    persistence: Cached
    storageClassName: local-path   # overrides --model-cache-storage-class
```

Behavior:

- In `perService` mode the field is authoritative for the claim the operator
  creates, `<isvc>-model-cache`.
- In `shared` mode the namespace has one claim shared by every service, so
  the first creator chooses its class.
- The class of an existing claim never changes: `storageClassName` is
  immutable once bound and the operator never mutates a claim. Editing the
  field against a claim that already exists (a differing later request in
  `shared` mode, or an edit on your own claim in `perService` mode) is
  ignored and raises a `ModelCacheStorageClassIgnored` warning event; delete
  the claim to reprovision on the requested class.
- The field cannot combine with `claimName` (the claim is user-owned) or with
  `persistence: Ephemeral` (no PVC is created to class); both are rejected at
  admission.
- There is no allowlist of storage classes: anyone who can edit an
  InferenceService can pin its cache claim to any storage class in the
  cluster. Scope who may edit InferenceServices if storage-class choice is
  restricted in your cluster.

## CLI Commands

### List Cached Models

```bash
# List cached models in default namespace
llmkube cache list

# List from all namespaces
llmkube cache list -A
```

Output:
```
Model Cache Entries
═══════════════════════════════════════════════════════════════════════════════
CACHE KEY         SIZE      MODELS              SOURCE
a3b8c9d4e5f67890  4.1 GiB   llama-2-7b          ...TheBloke/Llama-2-7B-GGUF/...
f1c314277254a2fd  7.2 GiB   llama-3.1-8b        ...meta-llama/Meta-Llama-3.1-8B/...

Total: 2 cache entries, 2 models
```

### Clear Cache

```bash
# Clear cache for a specific model
llmkube cache clear --model llama-2-7b

# Remove every entry that no Model references
llmkube cache clear --orphaned

# Clear all cache (with confirmation)
llmkube cache clear

# Force clear without confirmation
llmkube cache clear --force
```

Entries are removed by a short-lived `busybox:1.37.0` pod that mounts the cache
claim read-write, one pod per claim, deleted as soon as it is done. On a cluster
that cannot pull that image, pass `--cache-image <ref>` (or set
`LLMKUBE_CACHE_IMAGE`) to `cache list`, `cache clear` and `delete
--purge-cache`; the flag wins over the variable.

A `ReadWriteOncePod` claim another pod holds cannot be mounted a second time
on any node, so `cache list`, `cache clear` and `delete --purge-cache` refuse
it at once, naming the access mode and the holding pod, instead of waiting for
a pod that can never start. A pod that is already being deleted releases the
claim, so it is waited for, not refused, and so is the serving pod `delete
--purge-cache` itself is tearing down, which it recognises from the pod's
owner chain: the ReplicaSet that owns the pod, and the Deployment that owns
that ReplicaSet, must reach the service being deleted. For that check `cache
clear` and `delete --purge-cache` need `list pods`, and `delete --purge-cache`
also needs `get replicasets`; without either it falls back to the old wait,
while `cache list` reports the pod-list failure as it always has. A cache pod
llmkube itself left behind at the name the command does not use is named as
such: no run will free that claim, so the message says to delete the pod.
`cache list` pins its inspector pod only for a `ReadWriteOnce` or `ReadWriteOncePod`
claim: a `ReadWriteMany` or `ReadOnlyMany` claim is never pinned.

`cache clear` with no flag and `cache clear --orphaned` act on the claims
`cache list` discovers: `llmkube-model-cache` and the labeled per-service
`<isvc>-model-cache`. `cache clear --model` and `delete --purge-cache` target
only the claims that can hold that model's files: the
`spec.modelCache.claimName` of the InferenceServices that serve the model, the
`<isvc>-model-cache` of those services in `perService` mode, and the shared
cache, which is where their pods mount in the default `shared` mode (and the
only home when the Model has no InferenceService). Two things follow from that:

- It needs your own permission to get, create and delete pods in the
  namespace, and to exec into the one it creates. On a `restricted`-profile
  namespace the pod is rejected, the same way `cache list` is.
- A strict RWO claim that a serving pod still holds cannot be attached a second
  time on another node, so the purge fails with a timeout naming the claim.
  Delete the InferenceService first, then purge.

The namespace-wide modes remove only directories named like a cache key (16 hex
characters). `--model` and `delete --purge-cache` validate the key as a single
path segment first. Either way the command removes `/models/<key>` and never
data the operator did not write.

Clearing the entry of a model that is still serving removes the file from the
volume, but the running process keeps its open handle and serves the old bytes
until it restarts; the next start re-downloads.

`delete --purge-cache` differs from `cache clear --model` in one case: a
`perService` `<isvc>-model-cache` is owner-ref'd to the InferenceService, so the
delete reports that the claim goes with the service instead of purging it.

### Preload Models

Pre-download models before deploying them:

```bash
# Preload a catalog model
llmkube cache preload llama-3.1-8b

# Preload to a specific namespace
llmkube cache preload llama-3.1-8b -n production
```

This is useful for:
- Air-gapped environments (pre-populate cache on a connected machine)
- Reducing deployment time (model already cached)
- Bandwidth management (download during off-peak hours)

## Air-Gapped Deployments

For air-gapped environments:

1. **On a connected machine**, preload models:
   ```bash
   llmkube cache preload llama-3.1-8b
   llmkube cache preload mistral-7b
   ```

2. **Export the PVC.** The weights live in the cache PVC of the namespace you
   preloaded into, not in the controller pod. The controller's own `/models` is
   scratch space, and it never mounts a workload namespace's claim, so copying
   out of `llmkube-system` cannot reach them.

   Find the claim first. In `shared` mode (the default) it is
   `llmkube-model-cache`; in `perService` mode it is `<inferenceservice>-model-cache`:

   ```bash
   kubectl -n <namespace> get pvc
   ```

   Then mount it in a throwaway pod and copy from there:

   ```bash
   kubectl -n <namespace> run cache-export --restart=Never --image=busybox:1.36 \
     --overrides='{"spec":{"containers":[{"name":"cache-export","image":"busybox:1.36","command":["sleep","3600"],"volumeMounts":[{"name":"cache","mountPath":"/models"}]}],"volumes":[{"name":"cache","persistentVolumeClaim":{"claimName":"llmkube-model-cache"}}]}}'

   kubectl -n <namespace> wait --for=condition=Ready pod/cache-export --timeout=120s
   kubectl cp <namespace>/cache-export:/models ./model-cache
   kubectl -n <namespace> delete pod cache-export
   ```

   A ReadWriteOnce claim can only be mounted where it is already attached, so
   scale the InferenceService to zero first if the export pod will not schedule.

3. **On the air-gapped cluster**, import the cache by reversing the copy against
   a helper pod mounting the destination claim:

   ```bash
   kubectl -n <namespace> run cache-import --restart=Never --image=busybox:1.36 \
     --overrides='{"spec":{"containers":[{"name":"cache-import","image":"busybox:1.36","command":["sleep","3600"],"volumeMounts":[{"name":"cache","mountPath":"/models"}]}],"volumes":[{"name":"cache","persistentVolumeClaim":{"claimName":"llmkube-model-cache"}}]}}'

   kubectl -n <namespace> wait --for=condition=Ready pod/cache-import --timeout=120s
   kubectl cp ./model-cache/. <namespace>/cache-import:/models
   kubectl -n <namespace> delete pod cache-import
   ```

   The destination claim must exist before the copy. Deploy the model once and
   let it fail to download, or create the PVC yourself, so the operator has a
   claim to bind.

4. **Deploy models** (they'll be found in cache):
   ```bash
   llmkube deploy llama-3.1-8b --gpu
   ```

## Troubleshooting

### Model Not Using Cache

If models are still being downloaded via init container:

1. Check if the Model has a CacheKey:
   ```bash
   kubectl get model llama-3.1-8b -n <namespace> -o jsonpath='{.status.cacheKey}'
   ```

2. Verify the controller has cache enabled:
   ```bash
   kubectl get deploy -n llmkube-system llmkube-controller-manager -o yaml | grep model-cache
   ```

3. Check PVC exists in your namespace:
   ```bash
   kubectl get pvc llmkube-model-cache -n <namespace>
   ```

4. Check if model is cached in the PVC:
   ```bash
   kubectl exec -n <namespace> <pod-name> -- ls -la /models/
   ```

### Cache PVC Full

If the cache PVC runs out of space:

1. List cache entries:
   ```bash
   llmkube cache list -n <namespace>
   ```

2. Clear unused models:
   ```bash
   llmkube cache clear --orphaned -n <namespace>
   ```
   Then, if that is not enough, a specific model:
   ```bash
   llmkube cache clear --model <unused-model> -n <namespace>
   ```

3. Or resize the PVC (if your storage class supports it):
   ```bash
   kubectl patch pvc llmkube-model-cache -n <namespace> \
     -p '{"spec":{"resources":{"requests":{"storage":"200Gi"}}}}'
   ```

### Cache Corruption

If you suspect cache corruption:

1. Clear the specific cache entry, which removes the directory from the PVC:
   ```bash
   llmkube cache clear --model <model-name> --force -n <namespace>
   ```

2. Delete and recreate the InferenceService to trigger re-download:
   ```bash
   kubectl delete inferenceservice llama-3.1-8b -n <namespace>
   kubectl apply -f inferenceservice.yaml
   ```

   Or delete and recreate the Model:
   ```bash
   kubectl delete model llama-3.1-8b -n <namespace>
   kubectl apply -f model.yaml
   ```

## Performance Considerations

- **Storage Performance**: Use SSD-backed storage for faster model loading
- **Network**: For ReadWriteMany, ensure low-latency network between nodes and storage
- **Cache Size**: Plan for 1.5-2x your total model sizes to allow for cache rotation

## Disabling Cache

To disable persistent caching (not recommended):

```yaml
# values.yaml
modelCache:
  enabled: false
```

This will revert to the legacy behavior where each pod downloads the model via init container.

## Security Considerations

### Local and hostPath model sources (host-path allowlist)

Besides remote downloads, the model storage machinery can serve node-local sources: a Model whose `spec.source` is an absolute path (`/srv/models/model.gguf`) or a `file://` URL is mounted into the inference pod via a hostPath volume. As of v0.9.0, these sources are gated by an operator-configured allowlist ([GHSA-jw3m-8q7m-f35r](https://github.com/defilantech/LLMKube/security/advisories/GHSA-jw3m-8q7m-f35r)).

The allowlist is **empty by default**, which disables local and hostPath sources entirely. With the default configuration, a Model with a local source is rejected: it goes to phase `Failed` with a `SourceNotAllowed` condition, nothing is fetched, and no hostPath volume is created. Remote sources (`https://`, `pvc://`, `hf://`) are unaffected.

To keep serving models from node-local disk, list the absolute directory roots you trust:

```yaml
# values.yaml
modelSource:
  allowedHostPathRoots:
    - /srv/models
```

The equivalent controller flag is `--allowed-host-path-roots` (comma-separated). A source is permitted only when its cleaned path is within one of the configured roots; `..` escapes are rejected. The check is lexical and does not resolve symlinks, so only allowlist roots whose contents you control.

### Automatic shared cache on `fsGroupPolicy: None` backends

Whether Kubernetes applies the pod `fsGroup` to a volume depends on the CSI driver's `fsGroupPolicy` (`kubectl get csidriver <name> -o jsonpath='{.spec.fsGroupPolicy}'`), not on the filesystem. ceph-csi's CephFS driver defaults to `File`, so `fsGroup` is applied; a deployment can set its driver to `None` (some Rook CephFS installs do). NFS drivers vary, so check yours.

The automatic shared-model-cache workflow is a first-class supported path, **including** on `fsGroupPolicy: None` CSIs. On those backends Kubernetes does not apply the pod `fsGroup` to the volume, so the PVC root stays `root:root` and the non-root model downloader cannot write to it. The `model-cache-prep` init container fixes the ownership for you so the cache just works. You do not need to pre-stage models or hand-manage a PVC.

The prep is the **compatibility implementation** for these backends, and the operator adds it on every cache-backed pod and prefetch Job by default (`modelCache.prep: always`). On a `File` driver kubelet has already applied the ownership and the prep only re-asserts it, so it is redundant there: set `modelCache.prep: never` to drop it (see below). It runs as **non-privileged root (uid 0)** with `ALL` capabilities dropped and only `CHOWN`+`FOWNER` added (no privilege escalation, read-only rootfs, seccomp `RuntimeDefault`). Root here is a backend-driven requirement, not an avoidable misconfiguration: chowning a root-owned mount needs `CAP_CHOWN`, the prep shells out to `chown`, and a non-root process loses its capabilities across `execve` (Kubernetes has no ambient-capability field), so the exec'd `chown` would fail `EPERM`. Root retains its capabilities across exec. (Running the prep non-root broke the shared cache on these backends in 0.8.20; reverted in 0.8.21. A future change may move the chown into a small dedicated helper that performs the syscall in its own entrypoint, which would let the prep run non-root again without the exec capability loss.)

### Turning the prep off (`modelCache.prep`)

`modelCache.prep` (controller flag `--model-cache-prep`) selects whether the prep is injected:

| Value | Effect |
|-------|--------|
| `always` (default) | The prep runs in every cache-backed pod and every prefetch Job. |
| `never` | The prep is never injected. The downloader still runs non-root; if the volume's ownership is wrong it fails with `Permission denied` and the `ModelCacheNotWritable` diagnosis names this setting. |

Use `never` only when you know the cache's ownership is right without the prep: a CSI driver with `fsGroupPolicy: File` and a pod that sets `fsGroup` (kubelet applies only the pod's fsGroup), or a claim you pre-provisioned with the downloader's ownership. A supplied `spec.podSecurityContext` replaces the operator's default entirely, so `fsGroup` must be restated there. Then the cache-backed pods contain no root container at all, which is what a `restricted` namespace needs, and the prefetch Job additionally carries `runAsNonRoot` with a numeric uid, `allowPrivilegeEscalation: false`, `ALL` dropped, and a pod-level `seccompProfile: RuntimeDefault`.

Changing the value rolls every cache-backed serving pod once, because the pod template changes. A prefetch Job whose pod has already started finishes under the setting it started with. One that has not started a pod yet keeps the template it was created with, so under `restricted` its pods are refused until the Job is deleted and rebuilt (or its `ttlSecondsAfterFinished` expires). Under `never` the prefetch Job's containers run as uid 101, the default `curlimages/curl` user, whatever `--init-container-image` is set to.

`auto` (resolve the storage class and skip the prep only when the driver applies `fsGroup` itself) is a planned follow-up; the flag and the chart schema reject it today.

### PSA `restricted`

The prep needs root plus `CHOWN`/`FOWNER`, so a cache-backed pod with `modelCache.prep: always` cannot satisfy Pod Security Admission `restricted` (which forbids running as root and adding any capability except `NET_BIND_SERVICE`). On storage that applies `fsGroup` itself, set `modelCache.prep: never`: the root container is gone and the pod can pass `restricted`.

Each InferenceService sharing that cache must also supply, in `spec.podSecurityContext`:

```yaml
spec:
  podSecurityContext:
    runAsNonRoot: true
    runAsUser: 101        # numeric; the downloader inherits it
    fsGroup: 102          # the --default-fsgroup value; a supplied context replaces the operator default
    fsGroupChangePolicy: OnRootMismatch
    seccompProfile:
      type: RuntimeDefault
```

`fsGroup` has to be in the snippet because the operator's default `fsGroup` is not merged into a user-supplied context: without it kubelet applies no group to the cache volume and a `fsGroupPolicy: File` driver, which is why `never` was chosen, changes nothing. `OnRootMismatch` is the operator's own default for the same reason (see `fsGroupChangePolicy` below).

A numeric `runAsUser` is required, not optional: the default init image (`curlimages/curl`) declares the *named* user `curl_user`, and the kubelet refuses `runAsNonRoot` on an image whose user it cannot resolve numerically (`container has runAsNonRoot and image has non-numeric user (curl_user)`).

Inference pods deliberately do not get `runAsNonRoot` by default, because runtime images that run as root (vLLM, for example) would break. Without `never`, the alternatives are an `emptyDir` model store (no shared persistent cache) or a `baseline` namespace policy.

### `fsGroupChangePolicy` and pod start time on large caches

On storage where Kubernetes applies the pod `fsGroup` (CSI drivers with `fsGroupPolicy: File`, and under the default `ReadWriteOnceWithFSType` a `ReadWriteOnce` volume with an `fsType`), the default `fsGroupChangePolicy` is `Always`: before every pod start, kubelet walks the entire volume and changes the group and permissions of every file and directory. A shared model cache is the worst case for that walk, since it grows with every model you add, and on a network filesystem each change is a metadata round trip.

The operator therefore sets `fsGroupChangePolicy: OnRootMismatch` wherever it sets the default `fsGroup` (inference pods and the prefetch Job):

```yaml
securityContext:
  fsGroup: 102
  fsGroupChangePolicy: OnRootMismatch
  seccompProfile:
    type: RuntimeDefault
```

With `OnRootMismatch`, kubelet skips the walk when the volume root already has the `fsGroup` GID, group `rwx` and the setgid bit, which is the state the first full pass leaves and, under `modelCache.prep: always`, the `model-cache-prep` step (`chown 0:<fsGroup>`, `chmod g+rwX`) keeps. If anything changes the root's group, the next start does the full walk again. On `fsGroupPolicy: None` backends kubelet changes no ownership at all, so the setting has no effect there.

A user-supplied `spec.podSecurityContext` replaces the operator default entirely, so it does not get this setting. If you supply your own context on a large cache, add `fsGroupChangePolicy: OnRootMismatch` next to your `fsGroup`. When the default fsGroup is disabled (`--default-fsgroup=0`, the OpenShift preset), the operator sets neither field.

One consequence of skipping the walk: kubelet no longer re-grants group write on every file at each start, so files keep the modes their writer gave them. That is fine for the operator's own paths, because every downloader runs as the same init-image user and the serving containers mount the cache read-only. If you switch `--init-container-image` to an image with a different UID on a cache that is already populated, a resumed download into a directory the old UID created can fail with `permission denied`; delete that model's cache directory (or fix its ownership once) and let it download again.

### Shared-cache group-write multi-tenancy

In `shared` mode, the cache-prep init container (with its default `modelCache.prep: always`) sets `chmod g+rwX /models`, making the cache root writable by any pod in the same `fsGroup`. On a shared cache PVC, multiple InferenceServices share that `fsGroup` and can write to each other's cached model files.

Within one operator's trust domain this is fine. For a multi-tenant cache where distrusting tenants share the same namespace, this is a real consideration. Use per-service caches (`modelCache.mode: perService`) for isolation between distrusting tenants.

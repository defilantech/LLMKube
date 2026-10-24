---
title: Model cache and node-local storage
description: Choose shared, per-service, or pre-provisioned model storage, including strict-taint GPU nodes.
---

# Model cache and node-local storage

LLMKube downloads a remote Model through the `model-downloader` init
container in the inference pod. Persistent caching controls which PVC that
init container and the serving container mount.

## Cache modes

| Cluster/storage topology | Configuration |
| --- | --- |
| Single node | `shared` with the default RWO storage |
| Multi-node with RWX | `shared` with `accessMode: ReadWriteMany` and an RWX StorageClass |
| Multi-node without RWX, compatible topology-aware provisioner | `perService` |
| Strictly tainted node with unsuitable dynamic provisioner | Pre-provision a node-aligned PVC and set `spec.modelCache.claimName` |
| Mixed node types where the default class cannot attach everywhere | Set `spec.modelCache.storageClassName` per InferenceService |

`shared` is the default. A single cluster-wide PVC named `llmkube-model-cache`
is created (or reused if it already exists). When using an RWO storage class,
the shared claim binds to one node, so all inference pods scheduling onto that
PVC must land on the same node.

`perService` creates a dedicated PVC named `<inferenceservice>-model-cache` for
each InferenceService. Each per-service PVC uses RWO semantics and relies on the
StorageClass's `WaitForFirstConsumer` volume binding behavior to bind on the
node where the inference pod schedules. This avoids cross-node shared-RWO
affinity but does not affect external provisioner helper pods (see below).

### Per-service storage class

The cache class is an operator-wide choice (`modelCache.storageClass`), but an
InferenceService can override it for the claim the operator creates for it:

```yaml
spec:
  modelCache:
    storageClassName: local-path
```

The field chooses the class of the claim the operator creates; the class of
an existing claim never changes. In `shared` mode the first creator picks the
namespace's class, and in `perService` mode an edit to the field on a service
whose claim already exists is ignored. Either case raises a
`ModelCacheStorageClassIgnored` warning; delete the claim to reprovision on
the requested class. The field cannot combine with `claimName` (the claim is
user-owned) or `persistence: Ephemeral` (no PVC is created). There is no
allowlist of storage classes: anyone who can edit an InferenceService can
pin its cache claim to any storage class in the cluster.

## Strictly tainted GPU nodes

When a GPU node carries a hard `NoSchedule` taint, dynamic provisioning can
fail for reasons unrelated to the inference pod's tolerations. The safest
approach is to pre-provision a PVC bound to the tainted node and reference it
through `spec.modelCache.claimName`.

The inference pod already receives the GPU toleration derived from the Model's
`hardware.gpu` configuration. The `model-downloader` runs as an init container
in that same pod, so a pre-existing PVC does not require a separate download
Job.

The claim must already exist and be node-aligned by the cluster administrator's
storage setup. `claimName` is user-owned: LLMKube mounts it through the same
prep and download init containers as the built-in cache, but never creates,
mutates, or deletes it. If the named PVC is missing, the InferenceService is
marked Degraded rather than silently falling back to the shared cache.

Example with an AMD GPU node:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: amd-vulkan-model
spec:
  source: https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    gpu:
      enabled: true
      count: 1
      vendor: amd
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: amd-vulkan-service
spec:
  modelRef: amd-vulkan-model
  modelCache:
    claimName: gpu-node-model-cache
  resources:
    gpu: 1
```

The `source` URL above is an example. Replace it with an allowlisted, reachable
model source for your environment. This manifest does not create the PVC — the
PVC named `gpu-node-model-cache` must be pre-provisioned by the cluster
administrator and bound to a node that matches the GPU taint topology.

**Note:** `claimName` is ignored for `pvc://` model sources because those
weights are already staged on the cluster and mounted read-only; no download
occurs.

### Verification

After applying the Model and InferenceService, confirm the PVC is bound and
the inference pod lands on the expected node:

```bash
kubectl get pvc gpu-node-model-cache
kubectl get pod -l app=amd-vulkan-service \
  -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName
kubectl describe pod -l app=amd-vulkan-service
```

The PVC should show `Bound`, the pod should report the tainted GPU node, and
`describe` should list the correct toleration and volume mount.

### Warming a node-pinned claim with prefetch

A `WaitForFirstConsumer` local-path claim binds to whichever pod schedules
first. When `spec.prefetch: true` warms such a claim (#1676), that pod is the
prefetch Job, so the placement decision belongs there: set
`spec.prefetchNodeSelector` (and `spec.prefetchTolerations`, or a
`spec.prefetchAffinity` for what a selector cannot say) on the Model so the
download lands on the node that will serve it. Without a selector the PV can
bind to a node the InferenceService never runs on.

```yaml
spec:
  prefetch: true
  modelCache:
    claimName: model-cache      # the claim to warm; inherited by services without their own
  prefetchTolerations:
    - key: example-gpu
      operator: Exists
  prefetchNodeSelector:
    example.com/node-pool: gpu
```

Name the claim on the Model with `spec.modelCache.claimName` rather than
relying on the services to agree: it is the prefetch target whatever services
exist, and a service with no `modelCache` block of its own inherits it. Keep
the Job's placement able to reach that claim: a node-local claim is filled
only from its node, so pin the Job there rather than away from the GPU
nodes; a Job that cannot be scheduled is reported on the Model as
`PrefetchUnschedulable` and never finishes on its own. A
service can also require that its weights were staged before it starts, with
`spec.modelCache.prefetch: true`; it then creates no pods until the prefetch
into its claim succeeds, and its pods mount the claim read-only with no
downloader. See the Prefetch section of `docs/MODEL-CACHE.md` for the full
contract, including the `PrefetchTargetMismatch` and `PrefetchSkipped`
conditions.

Pair `modelCache.prefetch` with `spec.hubOffline: true` on the service: the Job
stages the checkpoint, and the serving container gets `HF_HUB_OFFLINE=1` and
`TRANSFORMERS_OFFLINE=1`, so any Hub lookup the operator did not stage (a
tokenizer in another repository, remote code, a draft or adapter named by
repository id) fails at start instead of downloading on the serving node. Leave
the operator staging the Model: a service with `skipModelInit: true`, TGI, or
`generic` without `stageModel` downloads the Model itself, cannot start under
`hubOffline`, and gets a `HubOfflineRuntimeDownload` Warning event. See the
air-gapped guide.

### Why a tolerated inference pod may still have a Pending PVC

Some dynamic provisioners create a per-node helper pod to provision volumes.
For example, MicroK8s hostpath deploys a `hostpath-provisioner-<node>-*` pod
on each node. This helper pod is separate from the LLMKube inference pod and
typically carries only the default not-ready/unreachable tolerations.

When a GPU node has a hard `NoSchedule` taint, the helper pod may remain
Pending with an untolerated-taint event — even though the inference pod itself
has the correct GPU toleration. A PVC has no pod `nodeSelector` or
`tolerations` field for LLMKube to set, and LLMKube does not manage the
external helper pod.

Strict-taint choices:

- Pre-provision a static, node-aligned claim and use `claimName`.
- Use a provisioner whose helper configuration supports the taint.
- Apply a narrowly scoped cluster-level policy maintained by the cluster
  administrator.

Global `perService` mode only avoids cross-node shared-RWO affinity. It cannot
make an untolerated external helper pod schedule on a tainted node.

## Gated and private Hugging Face repositories

Most vendor repositories (Llama, Gemma, Mistral and others) require an accepted
licence and an access token. Put the token in a Secret in the Model's namespace
and name that Secret in `spec.sourceSecretRef`. The downloader reads the
`HF_TOKEN` key and sends it as a bearer credential.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: hf-token
stringData:
  HF_TOKEN: hf_xxx
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-guard
spec:
  source: hf://meta-llama/Llama-Guard-4-12B
  format: safetensors
  files:
    - model.safetensors
    - config.json
    - tokenizer.json
  sourceSecretRef:
    name: hf-token
```

Public repositories need none of this. Omit the key, or the Secret entirely,
and downloads go out unauthenticated as before.

### Pulling from an authenticated Hugging Face mirror

A network that allows `huggingface.co` but blocks the CDN hosts that `resolve/`
redirects to can serve models from its own Hugging Face mirror instead
(Artifactory's `huggingfaceml` remotes are one). Point `spec.source` at the
mirror and add `HF_ENDPOINT` to the same Secret. A source whose host matches
`HF_ENDPOINT` is authenticated like a Hugging Face source, so the mirror's
bearer token is sent:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: huggingface
stringData:
  HF_ENDPOINT: https://artifactory.corp.example/artifactory/api/huggingfaceml/repo
  HF_TOKEN: <token>
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3-6-35b-a3b
spec:
  format: gguf
  source: https://artifactory.corp.example/artifactory/api/huggingfaceml/repo/unsloth/Qwen3.6-35B-A3B-GGUF/resolve/main/Qwen3.6-35B-A3B-UD-IQ4_NL.gguf
  sourceSecretRef:
    name: huggingface
```

The scheme, host and port must match, so the `/artifactory/api/huggingfaceml/...`
path is ignored but an `http://` source or a different port is not treated as the
same mirror (both would send the token somewhere the Secret did not name).
`HF_ENDPOINT` must be a full URL with a scheme. Leave `HF_ENDPOINT` out to keep
the previous behaviour (only `huggingface.co` is authenticated).

The value is trimmed, so a trailing newline from `kubectl create secret
--from-file` does not close the gate.

This covers every path the operator downloads on: single-file GGUF, multi-file
`spec.files` staging, `RefreshPolicy: OnChange` revalidation, the prefetch Job,
and the macOS Metal agent.

It also covers a runtime that downloads its own weights. A bare repo ID such as
`Qwen/Qwen3.8-27B-FP8` served by vLLM or SGLang with `spec.skipModelInit: true`,
or by TGI, never passes through the operator's downloader: the serving
container fetches it with `huggingface_hub`. When that Model's
`sourceSecretRef` names an `HF_ENDPOINT`, the serving container gets
`HF_ENDPOINT` (the trimmed value) so it downloads through the mirror too:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen38-27b-fp8
spec:
  source: Qwen/Qwen3.8-27B-FP8
  sourceSecretRef:
    name: huggingface        # holds HF_ENDPOINT and HF_TOKEN
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen38
spec:
  modelRef: qwen38-27b-fp8
  runtime: vllm
  skipModelInit: true
```

The token follows the same rule as the operator's downloads. The runtime sends
`HF_TOKEN` to `HF_ENDPOINT`, so:

- with no `vllmConfig.hfTokenSecretRef` (or the SGLang or TGI equivalent), the
  serving container gets `HF_TOKEN` from the Model's Secret, if it has one;
- with an `hfTokenSecretRef` in the **same** Secret, that token is used;
- with an `hfTokenSecretRef` in a **different** Secret, the mirror is not
  applied, because that token would go to a mirror its Secret never named. The
  runtime downloads from `huggingface.co` as before, and the InferenceService
  gets a `HFMirrorNotApplied` Warning event naming the fix.

A `spec.env` entry for `HF_ENDPOINT` or `HF_TOKEN` still wins.

`spec.hubOffline: true` drops this mirror env (`HF_ENDPOINT` and the token that
goes with it), because an offline runtime makes no request to send them to; see
the prefetch section above.

A Model can name one mirror origin. A speculative-decoding draft Model that
the runtime downloads itself uses the target Model's mirror, because both run
in the same container.

#### Staging a multi-file checkpoint through the mirror

Some mirrors break runtime downloads of large files. Artifactory's
`huggingfaceml` remotes answer a `HEAD` on an LFS file with a `302` to its own
URL; `huggingface_hub` follows same-host redirects, so the runtime fails with
`TooManyRedirects` (vLLM reports it as `Can't load tokenizer`). The operator's
downloader is unaffected, so let it stage the checkpoint instead: give the repo
ID as `spec.source` and select the files with `spec.files`. The runtime then
loads a local directory and never calls the Hub.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: glm-5-3-flash
spec:
  source: nvidia/GLM-5.3-Flash-NVFP4
  format: safetensors
  sourceSecretRef:
    name: huggingface        # holds HF_ENDPOINT and HF_TOKEN
  files:
    - config.json            # the primary: must match exactly one file
    - "*.json"
    - "*.jinja"
    - "*.safetensors"
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: glm
spec:
  modelRef: glm-5-3-flash
  runtime: vllm
```

With `spec.files`, a repo source (a bare repo ID, `hf://org/repo[@revision]`,
or a `huggingface.co` repo URL) is staged from `HF_ENDPOINT` when the
`sourceSecretRef` names one, and from `huggingface.co` otherwise, as
`huggingface_hub` treats `HF_ENDPOINT`. A bare repo ID **without**
`spec.files` is unchanged: the runtime downloads it. The mirror's own repo
base, `<HF_ENDPOINT>/<org>/<repo>/resolve/<revision>/`, also works as the
source; any other plain `https://` base is rejected for `spec.files`.

A repo source copied from the Hub with a trailing slash (`hf://org/repo/`,
`org/repo/`) is the same repository as the plain spelling: it lists, stages
and serves identically, and it shares one cache directory with it. The
`hf://org/repo` and bare `org/repo` forms keep separate cache directories, as
before, so no existing Model changes directory on upgrade.

Globs in `spec.files` are expanded against the repository's file listing,
which the Model controller reads from the Hub API of the same host the files
are downloaded from. Other sources need explicit paths.

- The expansion is recorded in `status.stagedFiles` when the Model reconciles
  a new generation. Files added upstream later are not picked up until the
  spec changes.
- `status.size` is the total of the staged files when the listing carries every
  staged file's size, the probed size for a remote GGUF or `s3://` source, and
  otherwise empty. An empty size means unknown, not zero.
- A pattern matches within one directory (`*.safetensors` does not match
  `sub/x.safetensors`; write `sub/*.safetensors`).
- A failed listing fails the Model with reason `RepoListingFailed` and the
  cause, and is retried every minute.
- A serving pod built before the expansion is recorded for its generation
  exits with `StagedFilesNotCurrent`, naming the Model whose status holds the
  cause, rather than blaming `spec.files`.
- A prefetch (`spec.prefetch`, or `modelCache.prefetch` on a service)
  expands the glob the same way before its Job is built, and records the
  expansion when it completes; the Job stages the expanded set.
- `spec.fileSha256` still needs explicit paths.

Properties worth knowing:

- **The token is only ever sent to `huggingface.co`, or to the origin named by
  `HF_ENDPOINT` in the same Secret.** A Model pointing at any other origin gets
  no header, even if the Secret it names carries an `HF_TOKEN` key, and the
  `HF_ENDPOINT` match is exact scheme, host and port, so a lookalike host, a
  downgrade to `http://` or another port cannot receive it. One Secret can safely
  hold both `HF_TOKEN` and the `AWS_*` keys.
- **A Model can name one mirror origin**, because `spec.sourceSecretRef` is a
  single Secret with a single `HF_ENDPOINT`. A fleet with two mirrors needs two
  Secrets.
- **It is dropped on a redirect that changes host.** Hugging Face answers a
  weights request with a redirect to a content host, and the token does not
  follow it.
- **An interrupted download behind a mirror that ignores `Range` restarts from
  zero.** Some remotes (Artifactory's `huggingfaceml` among them) answer a range
  request with the whole file and advertise no `Accept-Ranges`. The downloader
  then discards its partial rather than failing to resume it, so a restart is a
  full re-download instead of a pod that will not start.
- **A mirror that redirects a file to its own URL breaks runtime downloads.**
  An Artifactory `huggingfaceml` remote answers a `HEAD` on an LFS file with a
  `302` whose `Location` equals the requested URL, while small files answer
  `200`; `curl -sI <file URL>` shows it. `huggingface_hub` resolves file
  metadata with `HEAD` and follows same-host redirects, so a runtime fails
  with `TooManyRedirects`. On the remote this was verified against, `GET`
  returns `200`, and the operator's downloader stages those files: it tolerates
  the failed `HEAD` and downloads with `GET`. Stage such a model with
  `spec.files` (see above). If `GET` loops too, the downloader fails with
  `curl: (47) Maximum (50) redirects followed`. Either way it is mirror-side:
  take the `curl -sI` output to the mirror's administrators.
- **`refreshPolicy: OnChange` re-downloads on every start behind such a
  mirror.** Revalidation reads the size from that same `HEAD`; when it fails,
  the size is unknown and every file is downloaded again on each pod start.
  Keep the default `IfNotPresent`.

Rotating the token is a Secret edit; the new value is picked up the next time a
pod starts, the same as the S3 credentials. An `HF_ENDPOINT` change is also
picked up on the next reconcile or pod restart, not the moment the Secret is
edited.

## Custom servers on the generic runtime

`runtime: generic` runs your own image with your own `args`, so by default it
stages nothing: no `model-downloader` init container and no `/models` mount.
Set `spec.stageModel: true` to have the operator stage the referenced Model
exactly as it does for the built-in runtimes (same cache mode, credentials and
`spec.files` handling) and mount it read-only at `/models`.

The serving container then gets two environment variables. Kubernetes expands
`$(VAR)` in `args`, so point your server at them:

| Variable | Value |
|---|---|
| `LLMKUBE_MODEL_PATH` | The Model's primary file (the first entry of `spec.files`, or the single file). |
| `LLMKUBE_MODEL_DIR` | The staged directory of a multi-file Model, with `spec.files` paths preserved below it; for a single file, that file's directory. |

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: my-server
spec:
  modelRef: my-model          # spec.files: [weights/model-00001-of-00002.gguf, weights/model-00002-of-00002.gguf, draft/mtp.gguf]
  runtime: generic
  stageModel: true
  image: registry.example.com/my-server@sha256:...
  args:
  - --model
  - $(LLMKUBE_MODEL_PATH)
  - --draft-model
  - $(LLMKUBE_MODEL_DIR)/draft/mtp.gguf
```

`stageModel` and `skipModelInit` cannot both be true. Runtimes that stage by
default ignore `stageModel`.

`spec.imagePullPolicy` (`Always`, `IfNotPresent` or `Never`) sets the pull
policy of the serving container, which is the one that runs `spec.image`. It
does not touch the init containers, which run the operator's own images. Useful
for a custom image on a moving tag such as `:nightly`:

```yaml
spec:
  image: registry.example.com/my-server:nightly
  imagePullPolicy: Always
```

`Always` makes the kubelet pull whenever a pod is created. It does not watch the
registry, so a re-pushed `:nightly` still needs new pods; `kubectl rollout
restart deployment/my-server` does it. Leaving the field unset keeps
Kubernetes' own default (`Always` for a `:latest` or untagged image,
`IfNotPresent` otherwise), so upgrading the operator rolls no pod. A Metal
service runs no pod and ignores the field.

## Integrity verification (`spec.sha256`)

When a `Model` sets `spec.sha256`, the download init container verifies the
artifact against that digest before anything becomes the cache. A mismatch
fails the init container, so the pod never starts with bad weights.

- The transfer lands in a partial file and is renamed onto the final path only
  after the hash matches. On a mismatch the partial is discarded and the start
  fails, so bad bytes never become the cache.
- A rejected publish leaves a `<model>.<sha256>.sha256-rejected` marker, and
  the next start fails before any transfer, so a wrong pin does not
  re-download a multi-gigabyte artifact on every kubelet backoff. The marker
  is keyed on the expected digest, so correcting `spec.sha256` makes it inert
  and a co-tenant Model sharing the cache cannot clear it. Delete it to force
  a retry. This mirrors the metal-agent's `digestMismatchMemo`.
- With `RefreshPolicy: OnChange`, an upstream that moved past the pin keeps
  the pinned cached copy and exits 0; the marker then skips the re-fetch on
  later starts. The log names the digest mismatch, not reachability.
- A verified artifact gets a `<model>.sha256` stamp holding the digest plus
  the file size and modification time, so a stamp can only vouch for the exact
  bytes that were hashed. Later starts skip re-hashing while all three agree;
  a missing, stale or differently-sized file is hashed once and re-stamped.
  The stamp is written to a temp file and renamed, so a concurrent reader
  never sees a truncated one. The metal agent's model store writes and reads
  the same triple, so a stamp from either side is legible to the other, and a
  bare-digest stamp from an older agent release is hashed once and rewritten.
- A cached file that fails the re-hash is left in place (the cache directory
  may be shared) and replaced by a fresh verified download in the same start.
- The gates fail closed: if the digest does not reach the container, the
  download aborts instead of transferring unchecked.
- A pre-staged `pvc://` source with `spec.sha256` verifies the mounted file in
  an init container at pod start. The mount is read-only, so no stamp is
  written and the file is hashed on every pod start; a mismatch fails the pod.
  The hash is added to every pod start, rollout, reschedule and scale-out:
  expect roughly (file size / hash throughput), which for a 100-400 GB
  artifact on a network PVC can be many minutes. `oci://` stays
  digest-addressed and is rejected with `spec.sha256`.
- Combining `spec.sha256` with multi-file staging (`spec.files` /
  `spec.mmproj`) or an `oci://` source is rejected at admission; use
  `spec.fileSha256` for a multi-file set. Models that set such a combination
  before this release had the digest silently ignored; remove the ignored
  `spec.sha256` (or the multi-file / `oci://` source) to keep the Model
  admissible.

### Multi-file staging (`spec.fileSha256`)

`spec.sha256` attests to one artifact, so a multi-file set uses
`spec.fileSha256`, a map from repo-relative path to digest:

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

- Only listed files are verified; a file with no entry stages as before, so a
  partially pinned set is allowed. An unpinned shard in a partially pinned set
  gets no protection, so pinning the whole set is recommended.
- `spec.files` is capped at 256 entries of 512 characters each.
- A mismatch discards that file's partial, records its own
  `<file>.<sha256>.sha256-rejected` marker, and stops the loop. Once the
  upload is fixed, delete the marker (`<file>.<sha256>.sha256-rejected`) to
  let the next start re-download.
- Every key must name a `spec.files` entry or `spec.mmproj`. Globs in
  `spec.files` are rejected when `fileSha256` is set, because a digest cannot
  be keyed to an unknown expanded name. `spec.sha256` and `spec.fileSha256`
  are mutually exclusive, and `fileSha256` is rejected on an `oci://` source.

## Troubleshooting

### Pending PVC with `hostpath-provisioner-<node>-*` showing `untolerated taint`

The external provisioner's helper pod cannot schedule on the tainted node.
This is a limitation of the provisioner, not LLMKube. Use one of the
strict-taint choices above: pre-provision a static claim with `claimName`,
switch to a provisioner whose helpers tolerate the taint, or apply a
cluster-level policy.

### `volume node affinity conflict`

A shared RWO PVC is bound to a different node than where the inference pod is
trying to schedule. Options:

- Use an RWX StorageClass with the `shared` mode.
- Switch to `perService` so each InferenceService gets its own RWO PVC that
  binds via `WaitForFirstConsumer`.
- Pre-provision a node-aligned PVC and reference it with `claimName`.

### Slow pod starts that grow with the size of the cache

On storage where Kubernetes applies the pod `fsGroup`, its default
`fsGroupChangePolicy: Always` makes kubelet re-chown every file in the cache
before each pod start. LLMKube sets `fsGroupChangePolicy: OnRootMismatch` next
to the default `fsGroup` on inference pods and the prefetch Job, so after the
first pass kubelet skips that walk. A `spec.podSecurityContext` you supply
replaces the operator default, so add `fsGroupChangePolicy: OnRootMismatch`
next to your own `fsGroup` there.

### Cache inspection with `llmkube cache list`

The operator-managed shared and labeled per-service cache PVCs are discoverable
through the `app.kubernetes.io/component=model-cache` label. A user-managed
`claimName` PVC is not necessarily labeled as an operator cache; it remains
the user's responsibility to manage its lifecycle.

`cache list` reads each claim through a short-lived read-only `busybox` pod it
creates and deletes, rather than executing in a pod that happens to mount the
claim. In the default `shared` mode the controller mounts the shared claim and
its image has no shell, so borrowing that pod cannot work. The command needs
your own permission to list, get, create and delete pods in the namespace,
and `pods/exec` for the command it runs; it copies the mounting pod's tolerations,
so a tainted node does not reject it. When no discovered claim can be read it
exits nonzero and says why, instead of reporting an empty or fully active
cache. On a multi-node cluster the pod is pinned to the node holding the
claim, so a strict RWO serving pod does not make it stall on Multi-Attach; a
claim nothing holds is not pinned, and the pod is its first consumer.
`--all-namespaces` does not inspect claims and creates no pod.

### Removing cache entries with `llmkube cache clear`

`cache clear` removes entries instead of printing a command to run. One
short-lived `busybox` pod per cache claim mounts it read-write, removes only
validated `/models/<key>` paths, and is deleted straight after. `--model
<name>` clears one model on the claims its own InferenceServices mount (their
`spec.modelCache.claimName`, their `<isvc>-model-cache` in `perService` mode,
and the shared `llmkube-model-cache` in the default `shared` mode),
`--orphaned` clears every entry no Model references across the
label-discovered claims,
and `-n` picks the namespace. The command needs your own permission to create
and exec into pods there, and strict RWO storage refuses the second attach while
a serving pod holds the claim; delete the InferenceService first in that case.
`delete <name> --purge-cache` uses the same mechanism, except for a
`perService` claim the InferenceService owns: it is garbage-collected with the
service, so the delete reports it instead of purging it.

### Relaxing the taint

As an alternative to pre-provisioning, changing a GPU node's taint from
`NoSchedule` to `PreferNoSchedule` allows both the inference pod and the
external provisioner's helper pod to schedule with a scheduling penalty rather
than a hard block. This is a cluster-level trade-off: it increases scheduling
flexibility but reduces the guarantee that only tolerated workloads land on
GPU nodes.

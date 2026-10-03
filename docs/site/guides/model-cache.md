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

`shared` is the default. A single cluster-wide PVC named `llmkube-model-cache`
is created (or reused if it already exists). When using an RWO storage class,
the shared claim binds to one node, so all inference pods scheduling onto that
PVC must land on the same node.

`perService` creates a dedicated PVC named `<inferenceservice>-model-cache` for
each InferenceService. Each per-service PVC uses RWO semantics and relies on the
StorageClass's `WaitForFirstConsumer` volume binding behavior to bind on the
node where the inference pod schedules. This avoids cross-node shared-RWO
affinity but does not affect external provisioner helper pods (see below).

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
and the macOS Metal agent. It is separate from the runtime token settings
(`vllmConfig.hfTokenSecretRef` and its equivalents), which hand a token to the
serving container for the different case where the runtime downloads its own
weights from a bare repo ID.

Three properties worth knowing:

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

## Integrity verification (`spec.sha256`)

When a `Model` sets `spec.sha256`, the download init container verifies the
artifact against that digest before anything becomes the cache. A mismatch
fails the init container, so the pod never starts with bad weights.

- The transfer lands in a partial file and is renamed onto the final path only
  after the hash matches. On a mismatch the partial is kept and
  `<model>.sha256-rejected` records the rejected hash.
- A verified artifact gets a `<model>.sha256` stamp, so later starts skip
  re-hashing. A warm cache without a stamp is hashed once and stamped.
- A start against a rejected marker fails before any transfer. Correcting
  `spec.sha256` makes the marker inert; delete the marker to retry.
- `spec.sha256` applies to single-file Models; multi-file staging
  (`spec.files` / `spec.mmproj`) does not verify digests yet.

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

### Cache inspection with `llmkube cache list`

The operator-managed shared and labeled per-service cache PVCs are discoverable
through the `app.kubernetes.io/component=model-cache` label. A user-managed
`claimName` PVC is not necessarily labeled as an operator cache; it remains
the user's responsibility to manage its lifecycle.

### Relaxing the taint

As an alternative to pre-provisioning, changing a GPU node's taint from
`NoSchedule` to `PreferNoSchedule` allows both the inference pod and the
external provisioner's helper pod to schedule with a scheduling penalty rather
than a hard block. This is a cluster-level trade-off: it increases scheduling
flexibility but reduces the guarantee that only tolerated workloads land on
GPU nodes.

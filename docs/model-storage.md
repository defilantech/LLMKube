# Model storage

LLMKube serves each model's GGUF from a PersistentVolumeClaim mounted into the inference pod. How that cache is provisioned is controlled by `modelCache.mode`.

## Modes

### `shared` (default)

A single cluster-wide cache PVC (`llmkube-model-cache`) that the operator mounts and every inference pod shares, created by the operator in the InferenceService's namespace:

- The pod's **init container downloads each remote model into the shared PVC**, so all InferenceServices reuse one cache (**cross-service dedup**: a model is downloaded once and reused by every service that references it).
- `llmkube cache list` inspects this PVC, so cache inspection works out of the box.
- This is the proven default and is a drop-in for existing single-node clusters.

On a **multi-node** cluster, pair `shared` with **ReadWriteMany** storage (NFS, CephFS, EFS, etc.) so the cache is reachable from any node:

```yaml
modelCache:
  mode: shared
  accessMode: ReadWriteMany
  storageClass: <your-rwx-class>
```

With the default RWO storage class the shared PVC is pinned to one node, so it only works single-node (a GPU on any other node would hit a `volume node affinity conflict`). If your multi-node cluster has no RWX storage class, use `perService` instead.

### `perService` (opt-in escape hatch)

For multi-node clusters **without** an RWX storage class. Each InferenceService gets its own cache PVC (`<inferenceservice>-model-cache`):

- **RWO**, no explicit StorageClass, so it binds `WaitForFirstConsumer` — on whatever node the inference pod is scheduled to.
- The pod's **init container downloads the model into that PVC**, so the download and the server land on the same node by construction.
- Owner-referenced to the InferenceService, so it is garbage-collected when the service is deleted.
- This mode requires the external provisioner to schedule its helper path on the selected node. On a strictly tainted node where the helper cannot tolerate the taint, use `spec.modelCache.claimName` with a pre-provisioned claim instead (see below).

This makes **heterogeneous, multi-node clusters work without RWX**: an InferenceService whose accelerator is on node B (e.g. an AMD/Vulkan node distinct from the operator's node) schedules on node B and caches its model there (see #728).

```yaml
modelCache:
  mode: perService
```

Trade-offs: models are **not deduplicated across InferenceServices** (each service downloads and stores its own copy). Prefer `shared` + an RWX storage class on multi-node clusters that have one.

### Pre-provisioned claim (`spec.modelCache.claimName`)

Strict-taint users can use `spec.modelCache.claimName` with a pre-provisioned, node-aligned PVC. This path does not depend on dynamic helper scheduling: the claim already exists and is bound, so no external provisioner helper pod needs to land on the tainted node. The inference pod's init containers handle the download into the existing claim.

### Cache inspection

`llmkube cache list` discovers the shared cache and operator-managed per-service cache PVCs. A user-managed `spec.modelCache.claimName` PVC is outside the operator's cache label/discovery contract and may not appear in the listing.

## Choosing a mode

| Cluster | Recommendation |
| --- | --- |
| Single-node | `shared` (default) |
| Multi-node with an RWX storage class | `shared` + `accessMode: ReadWriteMany` + `storageClass: <rwx-class>` |
| Multi-node without RWX | `perService` |

## OCI image sources (`oci://`)

A model can be delivered as an OCI **container image** and mounted directly by
the Kubernetes **ImageVolume**:

```yaml
source: oci://registry.defilan.net/models/qwen3-32b@sha256:<digest>
```

The image is mounted read-only at `/model-source`, the same contract the
`pvc://` path uses. Because the kubelet and the node runtime pull and mount
it, an `oci://` model has **no downloader init container and no cache PVC**:
`spec.modelCache` is ignored for this source type, and the cache bug class
around PVC provisioning does not apply. Pull credentials come from
`InferenceService.spec.imagePullSecrets`, the same as any container image,
so private registries and air-gapped mirrors work with no extra configuration.

Pin by **digest** (`@sha256:...`) so a served model is immutable and
reproducible. A tag works but re-resolves when the pod restarts.

### What the image must contain

The reference must be a **container image** whose filesystem holds the weights, as
a `tar` / `tar+gzip` / `tar+zstd` layer. A raw OCI artifact (ModelPack, Docker
Model Artifact, an `oras push` with a model media type) is not a substitute: under
containerd the kubelet unpacks only tar image layers and mounts the volume
**empty** for any other layer media type (containerd issues #11381 / #11907).
CRI-O is more permissive; do not rely on that for a portable manifest.

The image's filesystem is mounted at `/model-source` and the served file is
`/model-source/<primary>`, where `<primary>` is

- `spec.files[0]` (or `spec.mmproj`), read as a path **relative to the image
  root**, when the file list is explicit; or
- `<Model.metadata.name>.gguf` (or the resolved `Status.GGUF.ModelName`) when
  `spec.files` is empty.

So put the weights at the **image root**, under the name LLMKube serves. The
KServe / Red Hat modelcar convention (`COPY weights /models/`) does not match: it
mounts at `/model-source/models/...`.

A minimal, correct image:

```dockerfile
FROM scratch
COPY ./qwen3-32b.gguf /
```

### Publishing models with OCI prefetch

Set `prefetch: true` and `prefetchOCI` to download an HTTP(S)/HF model and
publish it in the image layout above. Without `prefetchOCI`, prefetch keeps
its existing PVC behavior. See
[`config/samples/model_prefetch_oci.yaml`](../config/samples/model_prefetch_oci.yaml).

```yaml
spec:
  source: https://example.com/weights.gguf
  format: gguf
  prefetch: true
  prefetchOCI:
    image: registry.example.com/models/weights:v1
    ephemeralStorage: 10Gi
    pushSecretRef:
      name: registry-writer
```

`image` requires a fully qualified registry/repository and explicit tag, without
an `oci://` prefix. `pushSecretRef` is optional for anonymous registries; for
private registries it names a same-namespace `kubernetes.io/dockerconfigjson`
Secret with push permission. Only the publisher mounts this Secret. Source
credentials still use `sourceSecretRef`, only on the downloader. The configured
`--ca-cert-configmap` is trusted by both containers, including registry TLS.
Neither container needs a Kubernetes API token or privileged access.

The Helm chart uses its operator image for publishing. Other installations must
set `--oci-publisher-image=<operator-image>` to an image built from this version;
the Job runs `/manager publish-model` in that image. Private publisher/downloader
images need pull credentials on the Model namespace's default ServiceAccount.

The Job uses node-local `emptyDir` scratch space. `prefetchOCI.ephemeralStorage`
is a required positive Kubernetes quantity (for example `150Gi`). Set it above
**the total size of all selected files**, with headroom for partial downloads
and container logs. The controller applies the budget to the download and
publisher containers' `ephemeral-storage` requests and limits, and to
`emptyDir.sizeLimit`. Because the downloader is an init container, the scheduler
reserves the larger of the init-container and application-container requests,
not their sum. This makes disk usage visible to scheduling and gives the kubelet
a ceiling for the Job. Disk limits are enforced through eviction, not a hard
filesystem quota; leave node-level headroom as well. There is no default safe
for every model size. Existing OCI-prefetch manifests must add this field.

The publisher streams one filesystem tar layer per selected file, including
`mmproj`, and publishes a linux/amd64 + linux/arm64 image index sharing those
layers. Layers use gzip **stored blocks** (`gzip.NoCompression`), retaining the
`tar+gzip` media type without expensive compression of weights. Hashing and
upload still read the files multiple times, but no whole-layer memory cache is
used. Unchanged files retain their layer digests and are not uploaded again to
a registry that already holds them. Each individual file plus tar/gzip overhead
must fit the registry's blob-size limit; large single files still need upstream
sharding. This does not add resumable uploads across Job restarts.

Model prefetch node selectors, affinity and
tolerations apply to the Job. `spec.files`, `spec.mmproj`, source authentication
and checksum validation use the existing downloader. S3, local, PVC and existing
OCI sources are not accepted as OCI-prefetch inputs. Do not combine
`prefetchOCI` with Model-level `modelCache`; a service can still request its own
PVC staging through `InferenceService.spec.modelCache`.

Publication completes when `status.prefetchOCI.phase` is `Succeeded`.
`status.prefetchOCI.image` is the immutable `oci://...@sha256:...` reference;
`status.prefetchOCI.files` lists the paths inside the image, primary first.
Create a separate Model in the next environment using that reference and those
explicit `spec.files` paths (and `spec.mmproj`, when present). Explicit paths
also let the promoted Model have a different name. Omit download-only `sha256`
and `fileSha256` there: the OCI image digest pins the whole image.

Publishing does not rewrite `spec.source` or switch existing services to OCI.
An InferenceService using the original HTTP/HF Model still uses its normal
source/cache path. Only services using the promoted `oci://` Model require no
provider access. Publishing itself needs no ImageVolume support; consuming the
result has the cluster requirements below.

A changed source, selected file set, checksum or destination starts a new
publication. An unchanged successful publication is retained after the Job's
24-hour cleanup; upstream/tag changes are not polled. Use pinned source revisions
and unique destination tags for reproducibility. On failure, inspect the Job's
pod logs and `status.prefetchOCI.message`, then delete the failed Job to retry.
Removing `prefetchOCI` deletes its owned Job and clears publication status; it
does not delete registry images. Registry retention and cross-environment
promotion remain under your control.

For a cluster smoke test with tiny fixture weights (no GPU), rebuild and install
this operator image **and the regenerated CRDs** on a disposable cluster with
ImageVolume support. Confirm `--oci-publisher-image` names that rebuilt image
(the Helm chart sets it from its operator image). Use a unique image tag so the
node cannot reuse an older build. Then run:

```bash
KUBECONFIG=/path/to/disposable-cluster.kubeconfig \
OCI_PREFETCH_IMAGE=registry.example.com/models/oci-smoke:unique-test-tag \
OCI_PREFETCH_DOCKER_CONFIG=/path/to/docker-config.json \
bash -o pipefail -c 'bash hack/test-oci-prefetch.sh 2>&1 | tee /tmp/oci-prefetch-smoke.log'
```

The registry must be reachable from pods (push) and nodes (ImageVolume pull).
`OCI_PREFETCH_DOCKER_CONFIG` must include push access to the model repository and
pull access to the operator image if it is private. The script uses it for the
test namespace's default ServiceAccount and the serving InferenceService.
Omit it when both registries allow anonymous access. The test creates and
removes its own namespace, publishes fixture shards and a projector, removes
its HTTP provider, and creates an `oci://` Model plus a generic InferenceService
with `stageModel: true`. It checks the operator-created Deployment's ImageVolume
reference and verifies all three files over the serving Service. The output
includes the `oci://...@sha256:...` reference and a `BYTES CHECK PASSED` line for
each file. Keep `/tmp/oci-prefetch-smoke.log` as PR evidence. This exercises
publication, mounting and serving bytes, not LLM inference. The test leaves the registry tag for
inspection; delete it using your registry's normal retention tooling.

### Cluster requirement

`oci://` needs **Kubernetes >= 1.36** on the serving node (ImageVolume reached
GA there) and a supporting container runtime: **containerd >= 2.1.0** or
**CRI-O >= 1.31**. The capability lives on the node, not the API server, so a
node whose runtime cannot serve the volume fails the pod.

The operator refuses an `oci://` Model on a control plane below 1.36 with an
`UnsupportedCluster` condition naming the floor, rather than rendering a pod
that cannot start. Below the floor, use `pvc://`, `s3://`, or `hf://`, which
all work on older clusters and are the paths for edge, K3s, ARM, and Jetson
deployments.

See `docs/proposals/1379-oci-imagevolume-model-source.md` for the evaluation
behind this source type.

## Metadata

In `perService` mode the operator reads GGUF metadata (architecture, layer count, context length, etc.) for `Model.Status` by reading **only the file header** over HTTP range requests — it never downloads the whole model itself. The full model bytes are fetched only by the init container, on the serving node. `pvc://` and HuggingFace-repo sources are resolved at pod runtime and are unaffected.

## Multi-artifact staging

`spec.source` may name a repository or bucket **prefix** rather than a single object. In that case `spec.files` selects which artifacts to stage from it, and `files[0]` is the primary file handed to the runtime. This is how a sharded GGUF, an `mmproj`, or draft weights are expressed. All listed files land in **one cache directory**, so a sharded GGUF resolves its siblings by path.

```yaml
source: s3://models/org/Repo-GGUF
files:
  - Model-00001-of-00002.gguf
  - Model-00002-of-00002.gguf
mmproj: mmproj-model-f16.gguf
```

When `spec.files` is empty, `spec.source` must name a single object directly.

An `oci://` source carries every file in the image already, so
`spec.files` selects the primary file within that tree and nothing is fetched
per file. Use **explicit paths**: a glob cannot be expanded against an OCI
image, which provides no file listing, and the Model fails loudly rather
than serving a wrong path.

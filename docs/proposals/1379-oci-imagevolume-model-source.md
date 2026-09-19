# Evaluate OCI ImageVolume as a model source (#1379)

Status: proposal, awaiting a decision.
Author: LLMKube maintainers.
Related: #1379, #413 (model source context), #1110 (pinned multi-file models, closed), #1139 / #1353 / #928 (model-cache bug class).

## Recommendation

**Go, as an opt-in source type, gated on a modern cluster.** Add `oci://` as a model
source backed by the Kubernetes ImageVolume, and keep `pvc://`, `s3://`, and
`hf://` as the default and edge paths.

The conditions, all of which the rest of this document supports:

1. The feature is documented and treated as requiring **Kubernetes >= 1.36 on the
   node** and a supporting **container runtime** (containerd >= 2.1.0, or CRI-O >=
   1.31). It is not offered below that floor.
2. LLMKube does not raise its global supported floor. Edge, K3s, ARM, and Jetson
   users keep the existing PVC and object-store paths, which already work there.
3. The read-only model directory is accepted as the serving contract. It already
   is today (see "Why the read-only constraint is not new"), so this is a
   confirmation, not a behaviour change.
4. A capability gate surfaces an unsupported cluster as a clear status condition
   rather than a silently unschedulable pod.

If any of those conditions turns out to be unacceptable, the recommendation flips
to **no-go**, and the reasons are recorded below so the decision is reversible.

## What ImageVolume is

Kubernetes `ImageVolume` (KEP-4639, "OCI VolumeSource") mounts an OCI image or
artifact into a pod as a read-only directory. The API is a pod volume:

```yaml
spec:
  volumes:
    - name: model
      image:
        reference: registry.example.com/models/my-model@sha256:...
        pullPolicy: IfNotPresent
```

`ImageVolumeSource` has exactly two fields, `reference` and `pullPolicy`
(`Always`, `Never`, `IfNotPresent`; the default is `Always` for a `:latest`
reference and `IfNotPresent` otherwise). Pull credentials come from the same
mechanisms as container images: node credentials, ServiceAccount image-pull
secrets, and pod `imagePullSecrets`.
Source: <https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/>,
<https://kubernetes.io/docs/concepts/storage/volumes/>.

Maturity, from the Kubernetes feature-gates reference and KEP-4639:

| Release | Stage |
|---------|-------|
| v1.31 | Alpha, disabled by default |
| v1.33 | Beta, gained `subPath` / `subPathExpr` |
| v1.35 | Beta, enabled by default |
| v1.36 | **Stable / GA, enabled by default, no feature gate** |

Source: <https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/>,
<https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4639-oci-volume-source>.

## The real gate is the node, not the API server

This is the part that decides the feature, so it leads the risks. The API server
accepts a pod with `volumes[].image` even when the node's runtime cannot serve
it; a node whose runtime lacks support fails the pod rather than the admission.
The runtime support matrix:

| Component | Minimum for ImageVolume |
|-----------|-------------------------|
| Kubernetes (node/kubelet) | v1.31, GA at v1.36 |
| containerd | 2.1.0 (native support); 2.0 generally needs a patched build |
| CRI-O | v1.31 |

Source: <https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/>.

Consequence for the API design: a server-version check alone is not a sufficient
gate. The capability that matters lives on the node, which an operator cannot
reliably introspect. The Phase B design must therefore surface a clear failure
(the InferenceService does not become Ready, with a message naming the floor)
rather than assume support.

### Field test on a qualifying node

To confirm the floor empirically rather than only from documentation, a probe pod
was run on the LLMKube homelab:

```yaml
spec:
  nodeSelector:
    kubernetes.io/hostname: ahazidgx1
  volumes:
    - name: model
      image:
        reference: busybox:1.36
        pullPolicy: IfNotPresent
  containers:
    - name: prober
      image: curlimages/curl:8.18.0   # deliberately NOT the volume image
      volumeMounts:
        - name: model
          mountPath: /mnt/model
          readOnly: true
```

Node and result:

- Node `ahazidgx1`: kubelet `v1.36.2`, `containerd://2.2.3`, `arm64`, Ubuntu 24.04.4 LTS.
- Mount probe: `ls -l /mnt/model/bin/busybox` returned
  `-rwxr-xr-x 405 root root 1119816 May 18 2023 /mnt/model/bin/busybox`.
- Read-only probe: `touch /mnt/model/probe-write` returned
  `touch: /mnt/model/probe-write: Read-only file system` (exit 1).

So on a 1.36.2 node with containerd 2.2.3, an OCI image mounts as a read-only
directory, and the mount is genuinely read-only.

Two negative controls, so the probe asserts the mount and not the container:

- volume removed (emptyDir instead): `ls: /mnt/model/bin/busybox: No such file or
  directory` (exit 1), proving the positive result came from the ImageVolume.
- volume image set to the container's own image (`curlimages/curl`): the
  assertion would pass vacuously, which is why the probe uses two different
  images.

Not tested by this probe: a large model artifact. See "What this evaluation did
not prove".

## Fit against LLMKube's current model delivery

Today, a `Model` reaches a serving pod through a download path plus a model-cache
PVC, documented in `docs/model-storage.md`:

- `hf://`, `https://`, and `s3://` sources download through an init container
  (`internal/controller/model_storage.go`), with `pvc://` sources mounted
  pre-staged and never downloaded.
- The cache PVC is provisioned in one of three modes (`shared`, `perService`, or
  a user-supplied `spec.modelCache.claimName`), resolved by `modelCachePVCName`
  and `resolveCacheMode` (`internal/controller/model_storage.go`).
- The bug class this document cares about lives here: model-cache PVC handling
  has produced repeated defects (#928, #1139, #1353).

An `oci://` source removes two moving parts for that source type:

- **No downloader init container.** The artifact is delivered by the kubelet and
  the runtime, not by a `curl` in an init container, so the `remoteRevalidate`
  and multi-file download scripts (`buildModelInitCommand`,
  `buildMultiFileInitCommand`) do not apply.
- **No per-service cache PVC.** The volume is the image, read-only, with no
  claim to provision, bind, or garbage-collect.

It does **not** replace `pvc://`, `s3://`, or `hf://`. Air-gapped imports,
object stores, and pre-staged volumes all remain first-class. `oci://` is a
**third path**, and the proposal accepts that cost. The trade it buys is that for
the segment that can use it (sovereign and enterprise clusters, which run current
Kubernetes and have a registry or mirror), the cache bug class is not on the
critical path.

### Why the read-only constraint is not new

The usual objection to ImageVolume is that it is read-only and a model server
needs to write. In LLMKube this objection does not apply: the **serving container
already mounts the model read-only in every storage path**. The cache and emptyDir
paths mount `/models` read-only; the pre-staged PVC path mounts `/model-source`
read-only (`buildPVCStorageConfig`). Only the downloader init container writes to
the cache PVC, and an OCI source has no downloader.

So ImageVolume's intrinsic read-only semantics match the existing serving
contract exactly. The change is in how the bytes arrive, not in what the
server may do with them.

### Registry auth and the air-gapped story

Registry credentials and mirroring are the image path, unchanged:

- `InferenceService.spec.imagePullSecrets` already exists
  (`api/v1alpha1/inferenceservice_types.go`) and is applied to the pod spec
  (`internal/controller/deployment_builder.go`), so private registries work
  without new CRD surface.
- An air-gapped cluster mirrors the artifact the same way it mirrors images. The
  recommendation is to pin `oci://` references **by digest** (`@sha256:...`) so a
  served model is immutable and reproducible.

### Multi-file models

An ImageVolume merges the artifact's layers into one filesystem tree mounted at
`mountPath`; a subdirectory can be selected with `subPath`/`subPathExpr` on
Kubernetes 1.33 or later. LLMKube's `spec.files` / `spec.mmproj` staging plan
(`internal/controller/fileset.go`, `ResolveFileSet`) applies to that tree as it
does to a PVC.

One constraint must be recorded: `ResolveFileSet` expands globs against a
`repoFileList` that is fetched for Hugging Face sources. An OCI artifact provides
no such listing, so a glob in `spec.files` cannot be expanded for an `oci://`
source. The Phase B design either restricts `oci://` to explicit paths or fetches
a listing.

## Decision criteria

The recommendation follows from these, stated so it can be audited:

1. **Does the floor exclude a segment we claim?** Adopt only if the floor
   (1.36 + a supporting runtime) is acceptable for the sovereign and enterprise
   segments we sell, and the edge paths we already have are kept.
2. **Is the read-only model directory sound?** Yes, by construction: the serving
   container already mounts read-only (see above), and the field test confirms the
   mount is read-only on a 1.36 node.
3. **Is multi-file expressible?** Yes for explicit file lists; globs need a
   listing we do not get from an OCI artifact.
4. **Is the operational cost bounded?** Registry auth and mirroring reuse the
   image path; a large artifact's pull time and node storage are the operator's
   responsibility, and the doc says so rather than implying it is free.

If (1) fails for your fleet, this is a no-go on the floor, not on the mechanism.

## What this evaluation did not prove

- **Large artifacts.** The probe mounted a small image. A 100 GB-plus model's
  cold-pull time, node storage headroom, and warm-cache value were not measured.
  The general guidance is that ImageVolume suits large immutable artifacts on
  nodes with registry connectivity and image caching, and is not a streaming
  interface; a cold node pays the full download. This must be measured before
  anyone claims a startup-time win.
- **CI coverage.** Neither existing test tier exercises a real mount: envtest is
  API-only (it runs no kubelet), and the e2e suite uses `kind` at its latest
  release with no pinned 1.36 node image. Covering it in CI would need a pinned
  node image at 1.36 with containerd >= 2.1.
- **Non-containerd runtimes.** Only containerd was tested.

## Phase B scope (separate PR, only if the decision is go)

- `api/v1alpha1/model_types.go`: add `oci` to the `spec.source` Pattern (today
  `^(https?|file|pvc|hf|s3)://.*|...`), then `make manifests` and
  `make chart-crds`, and `git status` must be clean afterwards (AGENTS.md).
- `internal/controller/source.go`: `isOCISource` / `parseOCISource` beside
  `isPVCSource` and `isS3Source`.
- `internal/controller/model_storage.go`: `buildOCIStorageConfig`, dispatched in
  `buildModelStorageConfig` beside the `isPVCSource` branch. It returns one
  image-volume volume and one read-only `/model-source` mount, with no downloader
  init container and no cache PVC. The existing `buildPVCStorageConfig` is the
  shape to mirror.
- **Capability gate.** There is no server-version gate anywhere in the operator
  today; `internal/controller/crd_detector.go` gates on CRD presence only. Phase B
  chooses a mechanism (a documented prerequisite plus a clear status condition on
  pod-start failure, or a probe) and states it. It must not assume support.
- `docs/model-storage.md` and `docs/air-gapped-quickstart.md`: document the source
  type, the node floor, digest pinning, and mirroring.
- Tests: an envtest render test that `oci://` yields an image-volume volume and
  **zero** model-downloader init containers (fails if the dispatch is removed),
  and a gate test that an unsupported cluster is refused with a clear condition
  (fails if the gate is removed).

### Intended mapping

The shape is reviewable before any code exists. It mirrors `buildPVCStorageConfig`
exactly, because an OCI source is also pre-staged, read-only, and never downloaded:

```yaml
# Model
spec:
  source: oci://registry.defilan.net/models/qwen3-32b@sha256:<digest>
```

```yaml
# rendered serving pod (the parts that change)
spec:
  volumes:
    - name: model-source
      image:
        reference: registry.defilan.net/models/qwen3-32b@sha256:<digest>
        pullPolicy: IfNotPresent
  initContainers: []           # no model-cache-prep, no model-downloader
  containers:
    - name: llama-server
      volumeMounts:
        - name: model-source
          mountPath: /model-source
          readOnly: true       # same as the PVC path
```

`modelPath` becomes `/model-source/<path>`, the same convention
`buildPVCStorageConfig` uses, so the runtime args and `servedModelPath` do not
need to know which source type produced the bytes.

## Non-goals

- No change to the default for any existing source type.
- No replacement or deprecation of `pvc://`, `s3://`, or `hf://`.
- No new cache implementation. The OCI path has no cache, which is the point.

## Sources

- Kubernetes feature gates (ImageVolume stage per release):
  <https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/>
- Kubernetes volumes concepts (ImageVolume semantics, read-only, pull policy):
  <https://kubernetes.io/docs/concepts/storage/volumes/>
- Kubernetes API reference, pod v1 (`ImageVolumeSource`):
  <https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/>
- KEP-4639, OCI VolumeSource:
  <https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/4639-oci-volume-source>
- ImageVolume task page (runtime requirements):
  <https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/>

---
title: Air-gapped install
description: Deploy LLMKube and its models in clusters with no internet access. Covers offline operator install, private container registries, custom CA certificates, model storage strategies, serving vLLM and SGLang safetensors checkpoints without Hugging Face access, and using ModelRouter without external providers.
---

# Air-gapped install

LLMKube ships designed to install and operate in clusters with no
internet access. Government / defense, HIPAA-regulated healthcare,
financial trading floors, remote-edge sites, and any corporate
network with restrictive egress all fit the same shape: get the
operator image, the runtime image, the model weights, and a CA
bundle to the cluster once, then never reach the public internet
again.

This guide covers each piece end to end against the current `0.9.14`
release. Image tags on ghcr are bare versions, without a `v` prefix.

## Prerequisites

- A Kubernetes cluster (v1.30+) without public-internet egress
- A workstation with cluster access (`kubectl`, `helm`, optionally
  the `llmkube` CLI)
- A "sneakernet path": some way to copy a few tens of GB of files
  from a connected machine into the air-gapped environment
- One of: pre-downloaded GGUF model files or safetensors
  checkpoints, a private container registry the cluster can reach,
  or a `ReadOnlyMany` PVC seeded with model weights

## Architecture overview

Three workloads need to land inside the air gap:

1. **The operator** (`ghcr.io/defilantech/llmkube-controller:0.9.14`)
   reconciles `Model`, `InferenceService`, and `ModelRouter` custom
   resources.
2. **The runtime images** (`ghcr.io/ggml-org/llama.cpp:server-cuda13`
   for llama.cpp; equivalents for vLLM, TGI, Ollama). The operator
   only schedules pods using these; it doesn't pull weights into
   them.
3. **The router-proxy** (`ghcr.io/defilantech/llmkube-router-proxy:0.9.14`)
   if you use `ModelRouter`, or if you run Metal InferenceServices even
   with no `ModelRouter` at all: the controller's Metal relay pods use
   this same image. The release pipeline publishes a versioned
   router-proxy image alongside the controller on every release; mirror
   the tag that matches your controller version.

Plus the model weights themselves, which the operator either copies
from a local source or mounts from a PVC. There is no model-weight
download from the operator's runtime pods at request time.

## Step 1: Stage the container images

On a connected machine, save the four images you need:

```bash
docker pull ghcr.io/defilantech/llmkube-controller:0.9.14
docker pull ghcr.io/defilantech/llmkube-router-proxy:0.9.14
docker pull ghcr.io/ggml-org/llama.cpp:server-cuda13
docker pull docker.io/curlimages/curl:8.18.0   # init container

docker save \
  ghcr.io/defilantech/llmkube-controller:0.9.14 \
  ghcr.io/defilantech/llmkube-router-proxy:0.9.14 \
  ghcr.io/ggml-org/llama.cpp:server-cuda13 \
  docker.io/curlimages/curl:8.18.0 \
  > llmkube-bundle.tar
```

Transfer the bundle, then on the air-gapped side push to your
private registry:

```bash
docker load < llmkube-bundle.tar
for img in \
  ghcr.io/defilantech/llmkube-controller:0.9.14 \
  ghcr.io/defilantech/llmkube-router-proxy:0.9.14 \
  ghcr.io/ggml-org/llama.cpp:server-cuda13 \
  docker.io/curlimages/curl:8.18.0
do
  newtag="registry.internal.corp/${img#*/}"
  docker tag "$img" "$newtag"
  docker push "$newtag"
done
```

## Step 2: Install the operator from your private registry

Pull the Helm chart on the connected side:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm pull llmkube/llmkube --untar
```

Transfer the chart directory into the air gap, then install with
overrides pointing at your registry. If the cluster has no default
StorageClass, choose a cache configuration below before installing.

```bash
helm install llmkube ./llmkube \
  --namespace llmkube-system --create-namespace \
  --set controllerManager.image.repository=registry.internal.corp/defilantech/llmkube-controller \
  --set controllerManager.image.tag=0.9.14 \
  --set controllerManager.routerProxy.repository=registry.internal.corp/defilantech/llmkube-router-proxy \
  --set controllerManager.routerProxy.tag=0.9.14 \
  --set controllerManager.initContainer.repository=registry.internal.corp/curlimages/curl \
  --set controllerManager.initContainer.tag=8.18.0
```

The `initContainer.repository` / `initContainer.tag` overrides
matter: by default the InferenceService Pod schedules an init
container that pulls model weights via HTTP(S). In an air-gapped
environment that container still runs for every source that has to
be fetched or staged, including a `file://` path on the node. A
`pvc://` source is the exception: those weights are pre-staged and
mounted read-only, so no downloader runs at all (only a short
verify step, from the same image, when a single-file `pvc://` Model
sets `spec.sha256`). If your registry mirror requires a different image (your own distroless
curl, for example), point it here. The chart consumes these as two
separate fields, not a single `image` value (see
[`_helpers.tpl`](https://github.com/defilantech/LLMKube/blob/main/charts/llmkube/templates/_helpers.tpl)).

The same image runs the MetalNode dial-back verifier for Macs (see the
[macOS Metal guide](./macos-metal#the-dial-back)). For that it needs `sh`,
`mkdir`, `printf` and a curl of 7.57 or later (bracketed IPv6 addresses in
`--resolve`) built on a TLS library that accepts a self-signed server
certificate as its own trust anchor with `--cacert`, as OpenSSL and LibreSSL
do. `curlimages/curl` meets all of it. A curl that cannot fails closed: the
Mac shows `Reachable=False`, never a false `True`.

### Clusters without dynamic provisioning

The chart defaults to `modelCache.mode: shared`. The operator itself
mounts the shared cache PVC, so that claim must bind before the operator
can start, even when all Models use pre-staged `pvc://` weights.

For pre-staged `pvc://` Models on a cluster without dynamic provisioning,
add `--set modelCache.mode=perService` to the install command above. The
operator uses an `emptyDir` scratch volume and the chart creates no shared
cache PVC. The Models still mount their pre-provisioned source claims.
`perService` does not supply a storage provisioner: downloaded models
still need a usable cache StorageClass or an explicit
`InferenceService.spec.modelCache.claimName` in their own namespace.

To keep a persistent operator cache, create a PVC bound to a
pre-provisioned PV, such as a local PV on the data array. For example,
after provisioning a matching 100Gi, ReadWriteOnce PV named
`llmkube-cache-pv` with an empty storage class:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: operator-model-cache
  namespace: llmkube-system
spec:
  storageClassName: ""
  volumeName: llmkube-cache-pv
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 100Gi
```

Create the namespace and claim before installation. Then add
`-f cache-values.yaml` to the install command, with:

```yaml
modelCache:
  mode: shared
  existingClaim: operator-model-cache
```

The claim must be in the operator namespace and writable by the operator;
a local PV's node affinity must also allow the operator pod to schedule.
Helm mounts this claim without creating or managing it. This setting only
selects the operator's mount: inference cache claims are resolved in each
workload's namespace. To use a custom cache for downloaded weights, set
`spec.modelCache.claimName` on the InferenceService; `pvc://` Models keep
using the claim named in their source URL.

`llmkube cache list` discovers the shared operator cache by the conventional
`llmkube-model-cache` PVC name. If `existingClaim` names a differently named
PVC, that operator scratch cache will not appear in the list.

If dynamic provisioning is available but no class is the default, set
`modelCache.storageClass` to the desired class instead.

On live Helm installs and upgrades, `modelCache.requireDefaultStorageClass`
defaults to `true`: when shared caching has neither an explicit class nor
an existing claim, the chart checks for a default StorageClass and fails
with these three remedies if none exists. Offline `helm template` and
client-side dry runs skip this check because `lookup` has no API response;
use `--dry-run=server` to exercise it against a cluster. The Helm caller
needs permission to list StorageClasses. A chart-managed claim that is
already Bound skips the check on upgrade; a Pending one is still checked.
For the first install of a statically bound chart-managed claim, or a
caller without permission to list StorageClasses, set
`modelCache.requireDefaultStorageClass: false` and verify binding yourself.
The preflight checks the default annotation, not provisioner health or PV
capacity, and does not validate claims supplied through `existingClaim`.

### OpenShift / OKD / MicroShift

Use the bundled preset, which is air-gap-safe on top of being
SCC-compliant:

```bash
helm install llmkube ./llmkube \
  -f ./llmkube/values-openshift.yaml \
  --namespace llmkube-system --create-namespace \
  --set controllerManager.image.repository=registry.internal.corp/defilantech/llmkube-controller \
  --set controllerManager.image.tag=0.9.14 \
  --set controllerManager.initContainer.repository=registry.internal.corp/curlimages/curl \
  --set controllerManager.initContainer.tag=8.18.0
```

See [`OpenShift install`](./openshift-install) for the full
restricted-v2 SCC walkthrough.

## Step 3: Custom CA certificates

Most air-gapped corporate networks intercept TLS with an internal
CA. The operator's init container needs that CA in its trust store
to clone the model file over HTTPS from your internal model server.

The controller takes a `--ca-cert-configmap` flag naming a ConfigMap
that holds your CA bundle (PEM; the key name is free, `ca.crt` is the
convention). The Helm chart exposes it as
`controllerManager.caCertConfigMap`. The operator mounts that ConfigMap
into every model downloader it runs and combines it with the system
trust store, so public sources keep verifying alongside your internal
ones.

A ConfigMap volume is resolved in the pod's own namespace, so the
ConfigMap must exist in **every namespace where you create
`InferenceService` objects** (and in the namespace of any `Model` with
`spec.prefetch: true`). A copy in the operator's namespace alone is not
enough.

1. Create the ConfigMap in each workload namespace:

   ```bash
   kubectl -n default create configmap corporate-ca \
     --from-file=ca.crt=/path/to/corporate-root-ca.pem
   ```

   To manage the copies with the release instead, render them through
   the chart's `extraObjects`, one entry per namespace:

   ```yaml
   extraObjects:
     - apiVersion: v1
       kind: ConfigMap
       metadata:
         name: corporate-ca
         namespace: team-a
       data:
         ca.crt: |
           -----BEGIN CERTIFICATE-----
           ...
           -----END CERTIFICATE-----
   ```

2. Point the operator at it:

   ```bash
   helm upgrade llmkube ./llmkube \
     -n llmkube-system --reuse-values \
     --set controllerManager.caCertConfigMap=corporate-ca
   ```

3. Model downloads from `https://model-server.internal` or an internal
   `s3://` endpoint now verify against your CA without skipping TLS
   verification.

If the ConfigMap is missing from a workload namespace, the operator
does not create the serving pods (or the prefetch Job). Instead it
sets the InferenceService (or Model) to `Pending`, records a
`CACertConfigMapAvailable=False` condition with reason
`CACertConfigMapMissing` that names the ConfigMap and the namespace,
and emits a Warning event with the same text:

```bash
kubectl -n team-a get inferenceservice my-llm \
  -o jsonpath='{.status.conditions[?(@.type=="CACertConfigMapAvailable")].message}'
```

Copy the ConfigMap into that namespace and the service proceeds on
its own: the operator watches the ConfigMap and creates the workload
as soon as it appears. Holding the pods is deliberate. Without the
ConfigMap a pod cannot mount its CA volume, so it would sit in
`ContainerCreating` holding the GPU it was scheduled onto. Running the
downloader without the CA instead would only fail later, on TLS
verification against your internal endpoint.

## Step 4: Get model weights into the cluster

Pick one of four shapes. The PVC option is the most operationally
predictable for air-gapped fleets.

### Option A: `pvc://` (recommended)

Seed a `ReadOnlyMany` PVC once with the weights, then reference it
from every `Model`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: model-cache
  namespace: default
spec:
  accessModes: [ReadOnlyMany]
  resources: { requests: { storage: 100Gi } }
  storageClassName: nfs  # or whatever your air-gapped cluster has
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: llama-3-8b, namespace: default }
spec:
  source: pvc://model-cache/llama-3.1-8b-q4_k_m.gguf
  format: gguf
```

The controller validates the PVC exists and is Bound, marks the
`Model` Ready immediately, and the `InferenceService` mounts the
PVC read-only into the runtime pod. No init-container download, no
HTTP traffic.

### Option B: Internal HTTP server

If you already run a model server (NGINX serving GGUFs, an S3-
compatible internal bucket, an artifact registry):

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: llama-3-8b }
spec:
  source: https://model-server.internal.corp/llama-3.1-8b-q4_k_m.gguf
  format: gguf
  sha256: "a1b2c3d4e5f6...64-char-hex-string..."  # strongly recommended
```

The `sha256` field is the audit trail air-gapped operators need:
the controller computes the hash after copy and rejects the model
if it doesn't match. The computed value is written to
`status.sha256` either way.

### Option C: File path on the node

Lowest-ceremony option for single-node or DaemonSet shapes:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
spec:
  source: file:///mnt/models/llama-3.1-8b-q4_k_m.gguf
  format: gguf
```

**Breaking change in v0.9.0**: local sources (`file://` and
absolute paths) are disabled unless the path sits inside an
operator-configured allowlist of trusted directory roots
(GHSA-jw3m-8q7m-f35r). The allowlist is empty by default; with a
default install this option is rejected outright, and the Model
goes to phase `Failed` with a `SourceNotAllowed` condition (no
fetch, no hostPath volume). Add the model root to your Helm
values before using this option:

```yaml
modelSource:
  allowedHostPathRoots:
    - /mnt/models
```

or at install time with
`--set 'modelSource.allowedHostPathRoots[0]=/mnt/models'` (the
underlying controller flag is `--allowed-host-path-roots`,
comma-separated). The check is lexical: `..` escapes are
rejected, symlinks are not resolved, so only allowlist roots
whose contents you control. `https://`, `pvc://`, and `hf://`
sources are unaffected. If you upgrade with local-source
workloads already running, delete any InferenceService whose
source now falls outside the allowlist: a pod created before the
upgrade keeps its hostPath mount until it is deleted.

`allowedHostPathRoots` covers only the hostPath volume the
operator generates for a local `spec.source`. It does not inspect
an InferenceService's `spec.extraVolumes` or security contexts,
which reach the pod as written, so it does not stop an
InferenceService author from mounting another host path. Pod
Security Admission on the namespace is the control for that. Its
`baseline` level refuses every hostPath volume, including the one
this option generates, so a namespace that serves local sources
has to run at `privileged`: keep InferenceService and Model write
access there to principals you trust with the node. Under
`baseline`, serve node-local weights through a `pvc://` source
(Option A) bound to a `local` PersistentVolume instead. The
[trust model in SECURITY.md](https://github.com/defilantech/LLMKube/blob/main/SECURITY.md#trust-model)
has the details.

Pin the workload to nodes that have the file (via Deployment node
selector, taint/toleration, or affinity). The operator does not
distribute the file across nodes; that's the operator's
responsibility.

### Option D: `s3://` internal object store

If you already run MinIO, Ceph RGW, or any S3-compatible store
inside the perimeter, point the Model at it directly. Credentials
and the endpoint come from a Secret in the Model's namespace, wired
into the downloader as env:

```bash
kubectl -n default create secret generic minio-creds \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret> \
  --from-literal=AWS_REGION=us-east-1 \
  --from-literal=AWS_ENDPOINT_URL=https://minio.internal:9000
# Required for Metal (the metal-agent reads only a labeled Secret); harmless in-cluster.
kubectl -n default label secret minio-creds inference.llmkube.dev/model-source-credentials=true
```

```yaml
spec:
  source: s3://models/meta-llama/Llama-3.1-8B-Instruct-GGUF/Llama-3.1-8B-Instruct-Q4_K_M.gguf
  format: gguf
  sourceSecretRef:
    name: minio-creds
```

`spec.source` may also name a bucket **prefix** instead of a single
object, in which case `spec.files` selects which artifacts to stage
and `files[0]` is the primary file handed to the runtime. That is how
a sharded GGUF, an `mmproj`, or draft weights are expressed:

```yaml
spec:
  source: s3://models/org/Repo-GGUF
  files:
    - Model-00001-of-00002.gguf
    - Model-00002-of-00002.gguf
  mmproj: mmproj-model-f16.gguf
```

> `hf://org/repo` and bare `org/repo` sources do exist, but they
> download from `huggingface.co` (in the init container, or in the
> runtime itself), so they do not work inside an air gap. Mirror the
> files into one of the sources above instead. `spec.format` accepts
> `gguf`, `mlx`, `safetensors`, `pytorch`, or `custom`, and a Model
> that sets anything else is rejected at apply time.

## Step 5: Verify

```bash
kubectl -n default get models,inferenceservices
# expect: PHASE=Ready on the Model and on the InferenceService

kubectl -n default port-forward svc/llama-3-8b 8080:8080 &
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"llama-3-8b","messages":[{"role":"user","content":"hi"}]}'
```

If the call returns a token stream, the air-gap install is complete.

## vLLM and SGLang: safetensors checkpoints

Everything above uses a single GGUF file. vLLM and SGLang serve a
Hugging Face checkpoint instead: a directory with `config.json`, the
tokenizer files, and one or more `*.safetensors` shards. With a
security policy that blocks Hugging Face, neither an `hf://` source
nor a bare `org/repo` source can work, because both make the init
container or the runtime itself download from `huggingface.co`. The
directory has to be inside the perimeter before the `Model` exists.

LLMKube hands vLLM and SGLang a directory, not a file, in two cases:

- A `pvc://claim/dir` source. The claim is mounted read-only at
  `/model-source`, and the runtime gets `/model-source/dir`.
- A multi-file `s3://` source (a bucket prefix plus `spec.files`) with
  `spec.format: safetensors`. The init container stages every listed
  file into the model cache, and the runtime gets the staged directory
  rather than `files[0]`. Set the format explicitly: it defaults to
  `gguf`, and a GGUF Model hands the runtime `files[0]`.

An `oci://` source does not fit this shape today: it mounts the
artifact read-only at `/model-source`, but hands the runtime a file
path inside it (`files[0]`, or a `.gguf` basename when `files` is
empty) rather than the tree's root. Use `pvc://` or `s3://` for a
safetensors checkpoint.

The examples below use `Qwen/Qwen3-8B-FP8` (Apache 2.0, about 9.4 GB
of FP8 weights in two shards) pinned to commit
`220b46e3b2180893580a4454f21f22d3ebb187d3`. Substitute your own
model; the steps do not change.

### Stage and verify the checkpoint

On a machine that can reach the Hub, download one exact commit, never
a branch name, so that what you verify is what you ship:

```bash
REPO=Qwen/Qwen3-8B-FP8
REV=220b46e3b2180893580a4454f21f22d3ebb187d3
DIR=Qwen3-8B-FP8-${REV:0:8}

hf download "$REPO" --revision "$REV" --local-dir "$DIR"
rm -rf "$DIR/.cache"   # hf's own download bookkeeping, not part of the model
```

`--local-dir` writes plain files. Without it, `hf` builds a cache tree
whose `snapshots/<rev>/` entries are relative symlinks into
`../../blobs`, which dangle once the snapshot directory is copied or
mounted on its own.

Next, check every file against the digests the Hub publishes for that
commit. Recent releases of the `hf` CLI do this themselves:

```bash
hf cache verify "$REPO" --revision "$REV" --local-dir "$DIR" \
  --fail-on-missing-files --fail-on-extra-files
```

It exits non-zero on any mismatch. Run it after removing `.cache`:
`--fail-on-extra-files` counts that bookkeeping directory as files the
repo does not have.

If the transfer host cannot run `huggingface_hub`, this standard
library script makes the same check. The Hub's API lists each file of
a commit with a digest: a sha256 for LFS files (the weights and large
tokenizer files), and the git blob sha1 for small files such as
`config.json`.

```python
#!/usr/bin/env python3
"""usage: verify_hub.py REPO_ID REVISION LOCAL_DIR (stdlib only)"""
import hashlib, json, os, sys, urllib.request

repo, rev, root = sys.argv[1:4]
req = urllib.request.Request(
    f"https://huggingface.co/api/models/{repo}/revision/{rev}?blobs=true")
if os.environ.get("HF_TOKEN"):  # gated or private repos
    req.add_header("Authorization", f"Bearer {os.environ['HF_TOKEN']}")
info = json.load(urllib.request.urlopen(req))
if info["sha"] != rev:
    sys.exit(f"pin the full commit sha: {rev} resolves to {info['sha']}")

bad = 0
for f in info["siblings"]:
    name, path = f["rfilename"], os.path.join(root, f["rfilename"])
    if not os.path.isfile(path):
        print("MISSING ", name)
        bad += 1
        continue
    lfs = f.get("lfs")
    if lfs:
        h, want = hashlib.sha256(), lfs["sha256"]
    else:  # git blob sha1 of a small, non-LFS file
        h, want = hashlib.sha1(b"blob %d\0" % os.path.getsize(path)), f["blobId"]
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    ok = h.hexdigest() == want
    print("OK      " if ok else "MISMATCH", name)
    bad += not ok
sys.exit(1 if bad else 0)
```

Once the directory matches the Hub, record a sha256 for every file.
This manifest is what you check on the far side, where the Hub is not
reachable, and it is the source of the `spec.fileSha256` values in
option B:

```bash
(cd "$DIR" && find . -type f -print0 | sort -z | xargs -0 sha256sum) \
  > "$DIR.sha256"
```

(`shasum -a 256` stands in for `sha256sum` on macOS.) Carry the
directory and the manifest across together. On the target side, check
again before anything serves the weights:

```bash
cd /exports/models/Qwen3-8B-FP8-220b46e3 && sha256sum -c ../Qwen3-8B-FP8-220b46e3.sha256
```

Before you transfer, also look for custom model code:

```bash
grep -l '"auto_map"' "$DIR"/*.json; ls "$DIR"/*.py 2>/dev/null
```

No output means the architecture is built into vLLM and SGLang and
nothing executable rides along. If `auto_map` is present, the model
needs `trust_remote_code`, and the `*.py` files in the directory run
inside your serving pod: review them like any other third-party code
before they cross the boundary. An `auto_map` value of the form
`org/repo--module.Class` points at code in a different repository,
which the runtime will try to fetch from the Hub at load time; vendor
that code into the directory or choose another checkpoint. If you do
accept custom code, enable it explicitly: `extraArgs:
["--trust-remote-code"]` for vLLM, `sglangConfig.trustRemoteCode:
true` for SGLang.

### Option A: a PersistentVolume and a `pvc://` directory

Put the verified directory on storage the cluster can mount (an NFS
export here), expose it through a static PersistentVolume, and point
the `Model` at the directory inside the claim. Make the tree readable
by the runtime container's user; `chmod -R a+rX` is the simplest
choice for read-only weights.

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: models-nfs
spec:
  capacity: { storage: 500Gi }
  accessModes: [ReadOnlyMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  nfs:
    server: nfs.internal.corp
    path: /exports/models
    readOnly: true
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: models
  namespace: default
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: ""        # static binding: no provisioner involved
  volumeName: models-nfs
  resources: { requests: { storage: 500Gi } }
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3-8b-fp8
  namespace: default
spec:
  source: pvc://models/Qwen3-8B-FP8-220b46e3   # a directory, not a file
  format: safetensors
  hardware:
    accelerator: cuda
    gpu: { enabled: true, count: 1, vendor: nvidia }
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen3-8b-fp8
  namespace: default
spec:
  modelRef: qwen3-8b-fp8
  runtime: vllm
  # Your mirrored image, pinned by digest (see "Mirror the runtime images").
  image: registry.internal.corp/vllm/vllm-openai@sha256:<digest>
  vllmConfig:
    maxModelLen: 32768
  extraArgs: ["--served-model-name", "qwen3-8b-fp8"]
  hubOffline: true   # fail fast on any Hub lookup
  endpoint: { port: 8000, type: ClusterIP }
  resources: { gpu: 1, cpu: "8", memory: 32Gi }
```

The `Model` goes Ready as soon as the claim is Bound, with
`status.path` set to `/model-source/Qwen3-8B-FP8-220b46e3`. The pod
mounts the claim read-only and runs no downloader at all. Binding the
PVC to a named PV with `storageClassName: ""` keeps it out of any
`WaitForFirstConsumer` storage class, so the claim binds immediately.

For node-local disk, use a `local:` PersistentVolume with a
`nodeAffinity` naming the node, and the same pre-bound PVC. That is
a PV you create, not a host-path model source, so the
`modelSource.allowedHostPathRoots` allowlist (GHSA-jw3m-8q7m-f35r)
does not apply to it; the allowlist gates `file://` and absolute-path
values of `spec.source`, which Option C above documents for single
GGUF files. Pin the InferenceService to that node with `nodeSelector`.

Encoding the revision in the directory name means a new revision is a
new directory and a new `spec.source`, never an in-place overwrite of
weights a running pod has mapped.

Do not set `spec.sha256` on a `pvc://` directory. That field verifies
a single pre-staged file at every pod start, and the verify step fails
the pod when the path is a directory. `spec.files` and
`spec.fileSha256` are not accepted with a `pvc://` source either:
multi-file staging requires a Hugging Face, `s3://`, or `oci://`
source, and the Model goes `Failed` with `InvalidFileSet` otherwise.
For a `pvc://` checkpoint, the manifest check above, done when the
volume is seeded, is the integrity control. If you want the cluster to
verify every file on its own, use option B.

### Option B: an internal S3-compatible store with `spec.files`

Upload the verified directory under a prefix that includes the
revision, keeping relative paths:

```bash
aws s3 cp "$DIR" "s3://models/Qwen/$DIR" --recursive \
  --endpoint-url https://minio.internal:9000
```

Then list every file the runtime needs in `spec.files` and pin each
one in `spec.fileSha256`, using the values from the manifest. The
credentials Secret is the one from Option D above.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: qwen3-8b-fp8
  namespace: default
spec:
  source: s3://models/Qwen/Qwen3-8B-FP8-220b46e3
  format: safetensors
  sourceSecretRef:
    name: minio-creds
  files:
    - config.json
    - generation_config.json
    - model.safetensors.index.json
    - model-00001-of-00002.safetensors
    - model-00002-of-00002.safetensors
    - tokenizer.json
    - tokenizer_config.json
    - vocab.json
    - merges.txt
  fileSha256:
    model-00001-of-00002.safetensors: 83bb444edc7d4a1ed91ab67ad19620e2ed74724cdd62ede330fcaa7748c64404
    model-00002-of-00002.safetensors: c59d27856ca358b24770802ea52f1785258830f61ff3ac72a4b3896fad648f9b
    tokenizer.json: aeb13307a71acd8fe81861d94ad54ab689df773318809eed3cbe794b4492dae4
    # ...one line per file in spec.files, from the manifest
  hardware:
    accelerator: cuda
    gpu: { enabled: true, count: 1, vendor: nvidia }
```

The three digests shown are the Hub's own LFS sha256 values for that
commit, so they match your manifest exactly. The small files have no
Hub sha256 (only a git blob sha1), so take theirs from the manifest.
This turns the manifest into both lists:

```bash
sed 's|  \./| |' "$DIR.sha256" | awk '{print "    - " $2}'          # files
sed 's|  \./| |' "$DIR.sha256" | awk '{print "    " $2 ": " $1}'    # fileSha256
```

What the cluster then enforces:

- The downloader hashes each pinned file before it moves the file into
  the model cache. A mismatch discards that file's partial download,
  writes a `<file>.<sha256>.sha256-rejected` marker, and fails the init
  container, so the runtime never starts on bad bytes. Once the upload
  is fixed, delete the marker to let the next pod start retry.
- A cached copy is reused on later pod starts only if it still passes
  the same per-file check.
- Only listed files are verified. A file in `spec.files` without a
  `spec.fileSha256` entry is staged unchecked, so pin all of them.
- Admission rejects the combinations that cannot be verified: a
  `fileSha256` key that is not a `spec.files` entry (or
  `spec.mmproj`), a glob in `spec.files` while `fileSha256` is set,
  `fileSha256` without `spec.files`, and `spec.sha256` together with
  `spec.files`. Without `fileSha256` a glob is admitted but cannot be
  expanded, because there is no file listing to match it against, so
  staging fails; name every file. `spec.files` takes at most 256
  entries.

The `files[0]` entry is the primary file for GGUF runtimes; for vLLM
and SGLang with `format: safetensors` the runtime is handed the staged
directory, so the order of the list does not matter beyond `files[0]`
naming a real file. The files land in the model cache (see the
[`Model cache`](./model-cache) guide for sizing it), and the
InferenceService is the same as in option A.

If MinIO serves TLS from an internal CA, the downloader needs that CA
(Step 3).

### Mirror the runtime images and pin digests

A vLLM or SGLang service needs its runtime image inside the perimeter,
plus the curl init image from Step 1 when the source is `s3://` (a
`pvc://` directory source runs no init container). The defaults the
operator falls back to are compiled into each release (for example
`vllm/vllm-openai:v0.32.0`, `lmsysorg/sglang:v0.5.22-cu130`, and
`lmsysorg/sglang:v0.5.22-rocm720-mi30x` for AMD GPU Models), so set
them explicitly rather than relying on whatever your controller
version picks.

Pull the platform your GPU nodes run, which is not necessarily the
platform of the machine doing the pulling:

```bash
docker pull --platform linux/amd64 vllm/vllm-openai:v0.32.0
docker save vllm/vllm-openai:v0.32.0 > vllm-openai-v0.32.0.tar
# transfer, then on the air-gapped side:
docker load < vllm-openai-v0.32.0.tar
docker tag vllm/vllm-openai:v0.32.0 registry.internal.corp/vllm/vllm-openai:v0.32.0
docker push registry.internal.corp/vllm/vllm-openai:v0.32.0
crane digest registry.internal.corp/vllm/vllm-openai:v0.32.0
```

Pin the digest your registry reports. A `docker save`/`load` round
trip carries one platform, so that digest can differ from the
multi-arch index digest upstream publishes for the same tag; a
transfer that copies the whole index unchanged keeps the upstream
digest.

Then point the operator at the mirror for every service, fleet-wide:

```bash
helm upgrade llmkube ./llmkube -n llmkube-system --reuse-values \
  --set runtimeImages.vllm=registry.internal.corp/vllm/vllm-openai@sha256:<digest> \
  --set runtimeImages.sglang=registry.internal.corp/lmsysorg/sglang@sha256:<digest>
```

`runtimeImages.*` (the controller's `--runtime-images` flag) replaces
the built-in default for that runtime, including the vendor-specific
choices such as the SGLang ROCm image. An InferenceService
`spec.image`, as in the option A example, still wins over it. If the
registry needs credentials, set `spec.imagePullSecrets` on the
InferenceService.

### Gotchas

**`/dev/shm` for tensor parallel.** vLLM, SGLang and TGI move tensors
between GPU worker processes through shared memory, and the container
runtime's default `/dev/shm` (64 MiB) is far too small for more than
one worker. LLMKube mounts a memory-backed `emptyDir` at `/dev/shm`
for you when a single-node service runs several workers: more than one
GPU, or a tensor, pipeline or data-parallel size above 1. Its size cap
is half the container's memory limit, at most 16Gi (16Gi when no limit
is set). `spec.multiNode` ranks are left alone: they run with
`hostIPC` and use the host's `/dev/shm`.

To pick your own size, or to opt out, mount your own volume at
`/dev/shm`; it replaces the operator's:

```yaml
spec:
  extraVolumes:
    - name: dshm
      emptyDir: { medium: Memory, sizeLimit: 32Gi }
  extraVolumeMounts:
    - { name: dshm, mountPath: /dev/shm }
```

Pages written to a `medium: Memory` volume are charged to the pod's
memory, so `resources.memory` (or `memoryLimit`) has to cover the
runtime's own usage plus whatever lands in `/dev/shm`. The size cap
is a ceiling, not a reservation; size memory with it as headroom.

**Served model name.** vLLM and SGLang get `--served-model-name` set
to `spec.modelRef`, unless `extraArgs` already sets it, as in the
example. vLLM also keeps answering to the model path it is started with
(for example `/model-source/Qwen3-8B-FP8-220b46e3`), the id it served
before LLMKube set the flag, but reports the served name first and in
responses. Check with:

```bash
kubectl -n default port-forward svc/qwen3-8b-fp8 8000:8000 &
curl -sS http://localhost:8000/v1/models
```

**No Hub calls at serve time.** `spec.hubOffline: true` sets
`HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1` on the serving
container, so any Hugging Face lookup the runtime still attempts
(custom code, a tokenizer reference to another repo) fails immediately
with a clear error, instead of waiting out network timeouts against a
blocked host. The operator's own downloader init containers keep their
Hub access, since that is the path that stages the Model. An explicit
`spec.env` entry with the same name wins. The field does not apply on
the Metal agent: it runs no serving container, and the agent does not
forward `spec.env` either.

The field needs the operator to stage the Model, or a `pvc://` source.
A runtime that downloads the Model itself instead (`skipModelInit:
true`, TGI, `generic` without `stageModel`) fails at start under it,
and the InferenceService gets a `HubOfflineRuntimeDownload` Warning
event naming the fix.

**Leave `skipModelInit` unset.** The repo's vLLM samples that pull
straight from the Hub set `skipModelInit: true` and
`vllmConfig.hfTokenSecretRef`. Neither belongs in an air-gapped
service: with `skipModelInit` the `pvc://` or `s3://` storage is not
mounted, and vLLM is handed the raw `spec.source` string in place of
a mounted directory.

## ModelRouter in air-gapped clusters

The `ModelRouter` CRD enables policy-aware routing across multiple
backends. In an air-gapped cluster, "external provider" backends
target an internal LiteLLM proxy or a private-cloud inference
endpoint, not the public Anthropic / OpenAI APIs.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata: { name: secure-router, namespace: default }
spec:
  backends:
    - name: local-llama
      inferenceServiceRef: { name: llama-3-8b }
      tier: local
    - name: internal-litellm
      external:
        provider: litellm
        url: http://litellm.gateway.internal.corp:4000
        model: internal-claude-clone
        credentialsSecretRef: { name: litellm-key }
      tier: cloud   # tier name is internal; doesn't imply public internet
  rules:
    - name: pii-stays-local
      match: { dataClassification: [pii] }
      route: { backends: [local-llama] }
      failClosed: true
      timeout: 8s
  defaultRoute: local-llama
```

Two air-gap-relevant properties:

- **`failClosed: true`** on a rule rejects the request rather than
  falling through. In a regulated environment this is the
  enforcement boundary: PII can never spill onto a cloud-tier
  backend even if one is configured, because the controller
  validates that at apply time *and* the proxy enforces it at
  request time.
- **`provider: litellm`** with a private URL: LLMKube does not
  reach `api.litellm.ai`. The `provider` value selects the request-
  shape adapter; the `url` is the only thing dialed. Routes through
  your internal LiteLLM proxy, which then handles any further
  internal-cloud calls. See the
  [`ModelRouter concept doc`](../concepts/model-router) for the
  full policy model.

## Storage strategy

| Shape | Best for | Trade-off |
|---|---|---|
| `pvc://` with `ReadOnlyMany` NFS | Multi-node clusters, mixed read traffic | Requires shared storage |
| `pvc://` with `ReadWriteOnce` local SSD | Single-node, fastest cold-start | One pod at a time per PVC |
| Internal HTTPS server + SHA256 | Multi-cluster fleets that already have an artifact server | Controller computes hash on every download |
| Node-local `file://` | Single-node demos, DaemonSet shape | Requires pinning workload to nodes that have the file |
| Internal `s3://` + `spec.files` / `spec.fileSha256` | vLLM / SGLang safetensors checkpoints verified in-cluster | Files are copied into the model cache on first start |

For ModelRouter installations the proxy is stateless and doesn't
need its own storage. Only the runtime pods need model weight
access.

## Troubleshooting

**Model stuck in `Pending` with `init container exit 23`**
The init container couldn't reach the source URL. Confirm the
`--ca-cert-configmap` flag is set on the controller (see Step 3) if
your URL uses internal TLS, and that the named ConfigMap exists in
the InferenceService's namespace. View the init log:
`kubectl logs <pod> -c model-downloader`.

**InferenceService `Pending` with `CACertConfigMapAvailable=False`**
The operator runs with `--ca-cert-configmap`, but that ConfigMap is
not in the service's namespace, so no pod was created. Copy it into
the namespace named in the condition message (see Step 3); the
service proceeds automatically once it exists.

**Model stuck in `Pending` with `dial tcp ... no route to host`**
The cluster's egress policy is blocking the connection. Confirm
your `NetworkPolicy` permits the runtime namespace to reach
`model-server.internal.corp` on port 443.

**Model `Failed` with a `SourceNotAllowed` condition**
The `file://` or absolute-path source is outside the operator's
host-path allowlist, which is empty by default as of v0.9.0. Add
the model root to `modelSource.allowedHostPathRoots` (see Option C
above) and upgrade the Helm release, or switch the Model to a
`pvc://` source.

**`pvc://` source reports `PVC <name> not Bound`**
The PVC exists but no PV satisfies it. Check
`kubectl describe pvc <name>` for binding failures; storage class
mismatches are the most common cause in fresh air-gapped clusters.

**Operator pod `ImagePullBackOff`**
`controllerManager.image.repository` is wrong or the registry needs
auth. Add `imagePullSecrets` in the Helm values (the field is
top-level, not under `controllerManager`):

```bash
helm upgrade llmkube ./llmkube \
  -n llmkube-system --reuse-values \
  --set 'imagePullSecrets[0].name=internal-registry-creds'
```

## SHA256 audit and integrity

Every `Model` whose source is `https://` or `file://` gets a
`status.sha256` written by the controller after the download
completes. Pair this with the optional `spec.sha256` field for
fail-closed integrity verification:

```yaml
spec:
  source: https://model-server.internal.corp/model.gguf
  sha256: a1b2c3d4...
```

If the computed hash does not match `spec.sha256`, the controller
marks the `Model` `Failed` with the mismatch message in
`status.conditions[type=Ready].message`. The init container never
hands the bad file to a runtime pod.

`pvc://` sources don't get a `status.sha256` because the controller
doesn't mount PVCs at reconcile time. A `pvc://` source that names a
single file can still set `spec.sha256`: an init container hashes the
mounted file at every pod start and fails the pod on a mismatch. A
`pvc://` directory (a safetensors checkpoint) cannot be pinned this
way; verify it when you seed the volume, as described under
"vLLM and SGLang: safetensors checkpoints". Multi-file `s3://` and
Hugging Face sources pin each file with `spec.fileSha256` instead.

As of 0.10.1, a Metal InferenceService gets the same `spec.sha256`
enforcement from the metal-agent, for sources it downloads itself for
llama-server rather than the controller (local sources and the other
Metal runtimes are not checked): a mismatch deletes the file and refuses
to start the InferenceService with reason `ModelDigestMismatch`
instead of serving an unverified model. See "`spec.sha256` digest
verification" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#specsha256-digest-verification).

## Next steps

- [`OpenShift install`](./openshift-install) for restricted-v2 SCC
  clusters
- [`Multi-GPU sharding`](./multi-gpu) for 70B+ models across two
  or more GPUs
- [`Model cache`](./model-cache) for the persistent PVC cache
  pattern that complements pvc:// sources
- [`Model Router`](../concepts/model-router) for the policy /
  fail-closed semantics summarized above

## Reference

- [Helm chart values](../reference/helm-values)
- [CRD reference](../concepts/crds)
- [Architecture overview](../concepts/architecture)

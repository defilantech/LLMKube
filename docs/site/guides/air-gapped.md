---
title: Air-gapped install
description: Deploy LLMKube and its models in clusters with no internet access. Covers offline operator install, private container registries, custom CA certificates, model storage strategies, and using ModelRouter without external providers.
---

# Air-gapped install

LLMKube ships designed to install and operate in clusters with no
internet access. Government / defense, HIPAA-regulated healthcare,
financial trading floors, remote-edge sites, and any corporate
network with restrictive egress all fit the same shape: get the
operator image, the runtime image, the model weights, and a CA
bundle to the cluster once, then never reach the public internet
again.

This guide covers each piece end to end against the current `v0.7.7`
release.

## Prerequisites

- A Kubernetes cluster (v1.30+) without public-internet egress
- A workstation with cluster access (`kubectl`, `helm`, optionally
  the `llmkube` CLI)
- A "sneakernet path": some way to copy a few tens of GB of files
  from a connected machine into the air-gapped environment
- One of: pre-downloaded GGUF model files, a private container
  registry the cluster can reach, or a `ReadOnlyMany` PVC seeded
  with model weights

## Architecture overview

Three workloads need to land inside the air gap:

1. **The operator** (`ghcr.io/defilantech/llmkube-controller:v0.7.7`)
   reconciles `Model`, `InferenceService`, and `ModelRouter` custom
   resources.
2. **The runtime images** (`ghcr.io/ggml-org/llama.cpp:server-cuda13`
   for llama.cpp; equivalents for vLLM, TGI, Ollama). The operator
   only schedules pods using these; it doesn't pull weights into
   them.
3. **The router-proxy** (`ghcr.io/defilantech/llmkube-router-proxy:dev`)
   if you use `ModelRouter`. Only needed when you want the
   policy-aware routing layer. The release pipeline does not yet
   ship versioned router-proxy images alongside the controller, so
   the chart default is the `dev` tag (see
   [`#449`](https://github.com/defilantech/LLMKube/issues/449)). If
   you need a pinned tag in an air-gapped registry, build the image
   yourself from this commit (`make docker-build-router-proxy
   ROUTER_PROXY_IMG=registry.internal.corp/defilantech/llmkube-router-proxy:0.7.7`)
   and use that tag below.

Plus the model weights themselves, which the operator either copies
from a local source or mounts from a PVC. There is no model-weight
download from the operator's runtime pods at request time.

## Step 1: Stage the container images

On a connected machine, save the four images you need:

```bash
docker pull ghcr.io/defilantech/llmkube-controller:v0.7.7
docker pull ghcr.io/defilantech/llmkube-router-proxy:dev
docker pull ghcr.io/ggml-org/llama.cpp:server-cuda13
docker pull docker.io/curlimages/curl:8.18.0   # init container

docker save \
  ghcr.io/defilantech/llmkube-controller:v0.7.7 \
  ghcr.io/defilantech/llmkube-router-proxy:dev \
  ghcr.io/ggml-org/llama.cpp:server-cuda13 \
  docker.io/curlimages/curl:8.18.0 \
  > llmkube-bundle.tar
```

Transfer the bundle, then on the air-gapped side push to your
private registry:

```bash
docker load < llmkube-bundle.tar
for img in \
  ghcr.io/defilantech/llmkube-controller:v0.7.7 \
  ghcr.io/defilantech/llmkube-router-proxy:dev \
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
overrides pointing at your registry:

```bash
helm install llmkube ./llmkube \
  --namespace llmkube-system --create-namespace \
  --set controllerManager.image.repository=registry.internal.corp/defilantech/llmkube-controller \
  --set controllerManager.image.tag=v0.7.7 \
  --set controllerManager.routerProxy.repository=registry.internal.corp/defilantech/llmkube-router-proxy \
  --set controllerManager.routerProxy.tag=dev \
  --set controllerManager.initContainer.repository=registry.internal.corp/curlimages/curl \
  --set controllerManager.initContainer.tag=8.18.0
```

The `initContainer.repository` / `initContainer.tag` overrides
matter: by default the InferenceService Pod schedules an init
container that pulls model weights via HTTP(S). In an air-gapped
environment that container still runs (even when reading from a
`pvc://` or local file source — it sets up the on-disk layout). If
your registry mirror requires a different image (your own distroless
curl, for example), point it here. The chart consumes these as two
separate fields, not a single `image` value (see
[`_helpers.tpl`](https://github.com/defilantech/LLMKube/blob/main/charts/llmkube/templates/_helpers.tpl)).

### OpenShift / OKD / MicroShift

Use the bundled preset, which is air-gap-safe on top of being
SCC-compliant:

```bash
helm install llmkube ./llmkube \
  -f ./llmkube/values-openshift.yaml \
  --namespace llmkube-system --create-namespace \
  --set controllerManager.image.repository=registry.internal.corp/defilantech/llmkube-controller \
  --set controllerManager.image.tag=v0.7.7 \
  --set controllerManager.initContainer.repository=registry.internal.corp/curlimages/curl \
  --set controllerManager.initContainer.tag=8.18.0
```

See [`OpenShift install`](./openshift-install) for the full
restricted-v2 SCC walkthrough.

## Step 3: Custom CA certificates

Most air-gapped corporate networks intercept TLS with an internal
CA. The operator's init container needs that CA in its trust store
to clone the model file over HTTPS from your internal model server.

The controller binary accepts a `--ca-cert-configmap` flag that
takes the name of a ConfigMap holding a `ca.crt` key. The Helm
chart does not yet expose this as a top-level value (tracked
upstream as a documentation/chart-coverage gap), so you wire it in
by editing the controller Deployment args directly after install:

1. Create a `ConfigMap` in every namespace where you will deploy
   `InferenceService` objects (the init container runs in the
   workload's namespace, not the operator's):

   ```bash
   kubectl -n default create configmap corporate-ca \
     --from-file=ca.crt=/path/to/corporate-root-ca.pem
   ```

2. Patch the controller Deployment to pass the flag:

   ```bash
   kubectl -n llmkube-system patch deployment llmkube-controller-manager \
     --type=json \
     -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--ca-cert-configmap=corporate-ca"}]'
   ```

3. The operator now mounts that ConfigMap into every init container
   it spawns, so model downloads from `https://model-server.internal`
   succeed without skipping verification.

Repeat step 1 for each application namespace; the operator looks up
the ConfigMap in the InferenceService's own namespace at reconcile
time.

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

Pin the workload to nodes that have the file (via Deployment node
selector, taint/toleration, or affinity). The operator does not
distribute the file across nodes; that's the operator's
responsibility.

### Option D: HuggingFace repo ID (Ollama / vLLM only)

vLLM and Ollama runtimes can resolve HF repo IDs at runtime from a
mirror. Point your runtime image's `HF_ENDPOINT` environment
variable at your internal mirror (e.g. an Artifactory or Sonatype
Nexus repository) and use:

```yaml
spec:
  source: meta-llama/Meta-Llama-3.1-8B-Instruct
  format: hf-repo
```

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

**Model stuck in `Pending` with `dial tcp ... no route to host`**
The cluster's egress policy is blocking the connection. Confirm
your `NetworkPolicy` permits the runtime namespace to reach
`model-server.internal.corp` on port 443.

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
doesn't mount PVCs at reconcile time. Compute the hash externally
when seeding the PVC if you need provenance.

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

---
title: macOS Metal Agent
description: Run Metal-accelerated LLM inference on Apple Silicon Macs as a first-class Kubernetes workload. The Metal Agent is a native macOS daemon that watches the Kubernetes API and supervises llama-server processes with full Metal GPU access.
---

# macOS Metal Agent

LLMKube's Metal Agent is the thing no other Kubernetes LLM tool
does. It lets your Mac Studio, Mac mini, or any Apple Silicon
machine serve as a first-class Kubernetes inference node — with
the same `InferenceService` CRD you use for NVIDIA GPUs and
without losing access to Metal because the workload is trapped in
a container.

The shape: a native macOS daemon (the agent) watches the
Kubernetes API for `InferenceService` resources marked
`accelerator: metal`, spawns `llama-server` processes natively
with full Metal GPU access, and registers its authenticated TLS
ingress back into the cluster. The controller fronts that ingress
with an in-cluster relay, so any pod reaches your Mac over LAN /
Tailscale / WireGuard through an ordinary Service.

This guide gets you from a fresh Apple Silicon machine to a
running Metal-accelerated InferenceService in about ten minutes.

## Prerequisites

- **Apple Silicon Mac** (M1 / M2 / M3 / M4 / M5). Intel Macs with
  Metal 2+ work but performance is materially worse, and the bundled
  launchd plist points `--llama-server` at the Apple Silicon Homebrew
  path (`/opt/homebrew/bin/llama-server`), so change it to
  `/usr/local/bin/llama-server` there.
- **Access to a Kubernetes cluster** — either remote (recommended;
  most production patterns put the Mac on the LAN as an inference
  node) or local (minikube / kind on Docker Desktop).
- **kubectl** and **Helm** configured against your cluster.
- **Homebrew** (or any equivalent way to install `llama.cpp` with
  Metal support). Homebrew also installs the agent itself in Step 3.
- To build the agent from source instead: **Xcode Command Line Tools**
  (`xcode-select --install`, for `git` and `make`) and **Go 1.27.3 or
  newer** (the version `go.mod` pins).

## Step 1: Install llama.cpp with Metal

```bash
brew install llama.cpp
```

Verify Metal is detected:

```bash
system_profiler SPDisplaysDataType | grep Metal
# expected: Metal Support: Metal 3
```

## Step 2: Install the LLMKube operator in your cluster

If you haven't already, install the operator. The Metal Agent
relies on the operator's CRDs being installed and the controller
running.

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube \
  -n llmkube-system --create-namespace
```

For OpenShift clusters, add `-f values-openshift.yaml`
(see [`OpenShift install`](./openshift-install)).

## Step 3: Install and start the Metal Agent

### Install with Homebrew (recommended)

```bash
brew install defilantech/tap/llmkube defilantech/tap/llmkube-metal-agent
```

This installs the signed release binary as
`$(brew --prefix)/bin/llmkube-metal-agent`, `llama.cpp`, and the
`llmkube` CLI. Name both: the agent cask does not depend on the CLI
formula, because Homebrew trusts only the third-party tap items you
name on the command line (`brew trust`). Homebrew installs the latest
release, so keep the operator on the same release
(`brew info --cask llmkube-metal-agent` shows the version).

The cask does not start the agent. Joining the Mac to the cluster
writes the LaunchAgent and starts it, and does the RBAC part of step 4:
run `llmkube metal token create <mac>` where you have cluster access,
then the `llmkube metal join` line it prints, on the Mac. See
[Join a Mac](#join-a-mac).

### Build from source

To set the agent up by hand instead, clone the operator repo on your
Mac and check out the release that matches the operator Helm installed:

```bash
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
git checkout v$(helm show chart llmkube/llmkube | awk '/^appVersion:/{print $2}')
```

`helm show chart` reads the same local chart index `helm install`
used, so this builds the agent from the controller's release tag
instead of from `main`.

The agent does not need your admin credentials. From the checkout,
run the recipe in
["Dedicated kubeconfig with least-privilege RBAC"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#dedicated-kubeconfig-with-least-privilege-rbac).
It creates a least-privilege ServiceAccount, writes a kubeconfig for
it to `~/.kube/llmkube-metal-agent.yaml`, and ends by running the
bundled installer with that kubeconfig:

```bash
make install-metal-agent METAL_KUBECONFIG="$KC"   # KC=$HOME/.kube/llmkube-metal-agent.yaml
```

If you installed LLMKube with Helm, the chart can create that ServiceAccount
and its Role instead (`metalAgent.rbac`), so a `helm upgrade` keeps the agent's
permissions in step with the release. The kubeconfig is still minted by hand:
the chart never creates a token Secret. See the same README section for the
values block and the migration from the hand-applied manifest.

The recipe mints a 30-day token into a file that the kubeconfig reads through
`tokenFile`. To rotate it, mint a new token into the same file (the README
gives the one-line command); client-go re-reads the file about once a minute,
so the running agent picks it up without a restart or a plist change.
`llmkube metal join` is the recommended path: its token lasts 7 days and the
agent renews it itself.

Without `METAL_KUBECONFIG` the agent connects with the current context
of `~/.kube/config`, usually an admin identity. Under launchd it does
not see your shell's `$KUBECONFIG`.

This builds the agent binary, installs to
`/usr/local/bin/llmkube-metal-agent` (`sudo` asks for your password),
drops a launchd plist into `~/Library/LaunchAgents/`, and starts the
service. The build also runs `go fmt ./...` and `go vet ./...` over
the whole repository, and the first build downloads every Go module
dependency, so a first install takes noticeably longer than later
ones.

If you need to install manually (different binary path, different
launch system), see the
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md)
for the full plist and launchctl commands.

### Verify the agent is running

```bash
launchctl list | grep llmkube
# expected: <PID>   0   com.llmkube.metal-agent

curl -s http://localhost:9090/healthz
# expected: ok

tail -f ~/Library/Logs/llmkube/metal-agent.log
# leave this tab open; we'll watch it pick up the first InferenceService
```

## Step 4: Remote cluster setup

If your Kubernetes cluster runs on a different machine (a Linux
server or cloud cluster, as opposed to local kind / minikube), the
agent needs to register your Mac's reachable IP so the in-cluster
relay can reach the agent's ingress on your Mac.

```bash
# Find your Mac's IP on the LAN
ipconfig getifaddr en0
# example: 192.168.1.50

# Or on Tailscale / WireGuard
tailscale status | head -2
```

Edit `~/Library/LaunchAgents/com.llmkube.metal-agent.plist` and
add to the `ProgramArguments` array:

```xml
<string>--host-ip</string>
<string>192.168.1.50</string>
```

Reload. launchd keeps the definition it loaded, so unload the job,
wait for it to go away, and load the edited plist:

```bash
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

`make install-metal-agent` rewrites this plist from the repo's
template, so re-add your flags after a reinstall.

Restarting the agent does not stop the engines it started: the new agent
adopts them with the same PID and no model reload (oMLX services still
reload). Engines keep the flags they started with. To stop them, run
`llmkube-metal-agent stop-engines` once the agent has exited after the
`bootout` (`launchctl print` no longer finds it). Before rolling the
agent back to a release that predates adoption, do the same with the current
binary, or the older agent loads a second copy of every model. See the
[macOS deployment README](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#agent-restarts-upgrades-and-stopping-engines).

Without `--host-ip` the agent auto-detects an address: a Tailscale
address (100.64.0.0/10) if the Mac has one, otherwise the first other
routable IPv4 address (skipping loopback, link-local, and Docker or VM
bridge ranges). Only when there is none does it fall back, for a
cluster on the same Mac, to the IP address `host.minikube.internal`
resolves to on the Mac, else the one `host.docker.internal` resolves
to, else 192.168.65.254. It registers the address, never the
hostname, and logs
`no routable interface for host-IP auto-detect; using co-located DNS fallback`.
It logs the address it registered:

```bash
grep 'auto-detected host IP\|co-located DNS fallback\|registered endpoint' ~/Library/Logs/llmkube/metal-agent.log
```

The detected address can be one your cluster cannot reach (a personal
tailnet on a work Mac, or the wrong interface on a Mac with several),
so set `--host-ip` explicitly for a remote cluster, ideally to an
address with a DHCP reservation.

Engines on the Mac listen on `127.0.0.1` only. The cluster reaches
them through the agent's TLS ingress on `--ingress-port` (default
9443), so that is the one port to allow in the macOS firewall.

## Step 5: Deploy a model with Metal

From any machine that can talk to your cluster:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: phi-4-mini }
spec:
  source: https://huggingface.co/bartowski/microsoft_Phi-4-mini-instruct-GGUF/resolve/main/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: phi-4-mini }
spec:
  modelRef: phi-4-mini
```

```bash
kubectl apply -f phi-4-mini.yaml
kubectl get inferenceservice phi-4-mini -w
# wait for PHASE=Ready
```

A public Hugging Face URL like the one above works out of the box. If
`spec.source` instead points at a host on your LAN (an internal MinIO or
model mirror), add it to the agent's `--allowed-download-hosts` first: the
agent refuses to download from a private or loopback address unless it is
allowlisted. Set `spec.sha256` on the `Model` to have the agent verify the
download (llama-server sources the agent downloads) and refuse to start on a
mismatch. See "Security model" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#security-model)
for both.

A private source needs credentials in a Secret named by the Model's
`spec.sourceSecretRef` (`AWS_*` for `s3`, `HF_TOKEN` for a gated Hugging
Face repository). The agent reads that Secret only when its owner has
labeled it for this use, and never reads the relay token Secret
`llmkube-metal-relay`; any other Secret refuses the start with
`SourceSecretNotAllowed`. The agent re-checks a refused service on a
backoff (5 seconds, doubling to once a minute), so once the Secret is
labeled the service starts within about a minute, with no change to
the InferenceService:

```bash
kubectl label secret <name> -n <namespace> inference.llmkube.dev/model-source-credentials=true
```

See ["Model-source credentials Secrets"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#model-source-credentials-secrets-sourcesecretref)
in `deployment/macos/README.md`.

The agent's log should show:

```
"msg":"starting inference service","name":"phi-4-mini"
"msg":"registered endpoint","service":"phi-4-mini-agent","hostIP":"192.168.1.50","port":9443,"enginePort":<allocated>
"msg":"started inference service","name":"phi-4-mini","pid":<llama-server-pid>
```

### How traffic reaches the Mac

The agent starts `llama-server` on `127.0.0.1` and registers its
TLS ingress as the Service and EndpointSlice `phi-4-mini-agent`.
The controller then creates a relay Deployment `phi-4-mini-relay`
in the cluster and points the `phi-4-mini` Service at it. The relay
pins the agent's certificate and authenticates with a per-namespace
token; the ingress forwards only an allowlist of inference paths to
the engine. Clients never need the Mac's IP or the engine port.

```bash
kubectl get svc phi-4-mini
kubectl get pods -l inference.llmkube.dev/service=phi-4-mini
# the relay pod: phi-4-mini-relay-<hash>
```

### Query the model

From a pod in the cluster, call the Service like any other
InferenceService. The port follows `spec.endpoint.port` (default
8080). Send the name `GET /v1/models` lists as the `model` field. That
is the served model name: the InferenceService's `spec.modelRef`, not
the InferenceService name. In this guide the Model, `modelRef` and
InferenceService all share the name `phi-4-mini`, so the two look the
same here; they differ when you name the InferenceService differently
from its Model.

```bash
kubectl run curl --rm -it --restart=Never --image=curlimages/curl -- \
  curl -sS http://phi-4-mini.default.svc:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

If your shell or paste medium strips backslash line-continuations,
the equivalent one-liner is safer to copy:

```bash
kubectl run curl --rm -it --restart=Never --image=curlimages/curl -- curl -sS http://phi-4-mini.default.svc:8080/v1/chat/completions -H 'content-type: application/json' -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

On the Mac that runs metal-agent, use the agent's client proxy on
`127.0.0.1:9999` (set with `--client-port`), which forwards `/v1/*`
to the running engine:

```bash
curl -sS http://127.0.0.1:9999/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

oMLX uses the same name. The agent registers `spec.modelRef` as the
model's oMLX alias, so `/v1/models` lists it and requests that send it
reach the model. The model-store directory name also keeps working. See
the "Model identifier" section of the
[macOS agent guide](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#model-identifier).

### Reaching the service from elsewhere

The `phi-4-mini` Service selects the relay pod, so the usual tools
work:

1. **Port-forward** from a workstation with cluster access:

   ```bash
   kubectl port-forward svc/phi-4-mini 8080:8080
   curl -sS http://localhost:8080/v1/chat/completions \
     -H 'content-type: application/json' \
     -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
   ```
2. **Set `spec.endpoint.type: NodePort`** (and optionally
   `spec.endpoint.nodePort`) on the InferenceService for a stable,
   externally reachable port on the cluster's nodes.

Dialling `<mac-ip>:<engine-port>` directly does not work: the
engine is not listening on the network. For what the ingress
authenticates, the path allowlist, token and key rotation, and the
upgrade order, see "Network exposure" in the
[`deployment/macos/README.md` security model](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#network-exposure).

## Memory budgets

The agent estimates each model's memory cost (weights + KV cache +
overhead) before spawning `llama-server`. If the model won't fit
in the configured budget, the agent refuses to start it and marks
the InferenceService with `status.schedulingStatus: InsufficientMemory`.
Like every refusal, it is re-checked on a backoff of up to a minute,
so the service starts once memory frees up; a refusal that holds
produces one Warning Event, not one per re-check.

The KV-cache term is sized from the model's GGUF attention shape
(KV head count and key/value length, i.e. grouped-query attention)
when the file provides it. The charge can be lower or higher than the
old embedding-width formula: lower for GQA models whose KV head count
is well below the attention head count (Llama 3.1), higher for
head-dim models whose heads times head_dim exceeds the embedding
width (Gemma), so a model admitted under the old estimate can be
refused after this change. When the GGUF omits the key/value lengths,
the head dims are derived as the embedding width divided by the
attention head count, which is what llama-server itself uses for such
files (Llama 3.1 and Qwen 2.5 GGUFs carry the KV head count but not
the lengths). The embedding-width fallback remains for GGUFs without
a usable attention shape at all: no scalar KV head count (absent, or
stored per layer as an array on hybrid archs), no scalar head count,
more KV heads than attention heads, or a head count that does not
divide the embedding width when a key or value length is missing from
the file.
Two known over-charges remain by design: models with sliding-window
layers (Gemma 3) are charged the full-attention head dims for every
layer, and DeepSeek MLA-layout files are charged their stated head
dims even though llama-server stores only the compressed key cache,
so they are over-charged. Older deepseek2 conversions carry plain
head dims and no `*_mla` keys, run the non-MLA path, and are charged
exactly what llama-server allocates.

The estimate charges the total `--ctx-size` pool: llama-server
shares that pool across `--parallel` slots rather than multiplying
it, so admission does not scale with slot count.

A Model parsed by an older controller keeps the
embedding-width estimate until its GGUF metadata is re-parsed, which
happens on the next download, or after any change to the Model's spec
when the Available condition records an older generation than the
spec (a Ready status written before that condition existed has no
such record and needs the Model recreated). Editing a local source's
file on disk does nothing under the default `IfNotPresent` refresh
policy; with `refreshPolicy: OnChange` the size and mtime change
counts as drift, triggering a re-download and a fresh parse.

Defaults are tuned by total system RAM:

| Total RAM | Default fraction | Budget |
|---|---|---|
| 16 GB | 67% | ~10.7 GB |
| 36 GB | 67% | ~24.1 GB |
| 48 GB | 75% | 36 GB |
| 64 GB | 75% | 48 GB |
| 128 GB | 75% | 96 GB |

Override the fraction with `--memory-fraction 0.9` for a dedicated
inference machine, or `0.5` if the Mac is also your daily-driver
workstation. Add the flag to the launchd plist's `ProgramArguments`
the same way as `--host-ip`.

If the agent cannot complete the check at all (the model is not on
disk yet, the Model's `status.size` is not populated, and a HEAD
probe of the source fails), it **fails closed**: the process is not
started and the InferenceService is marked with
`status.schedulingStatus: MemoryCheckFailed` explaining why. On
Apple Silicon an unsized model is wired into unevictable memory, so
admitting it blind risks a host-level stall instead of a failed pod.
Set `--memory-check-mode warn` to restore the older log-and-proceed
behavior if you need an escape hatch.

A size probe that fails only because the source cannot be reached
right now (connection refused, no route to host, a timeout, a
temporary DNS failure, a 5xx or a 429) is retried on the next poll
without a status change or a Warning Event; nothing starts in the
meantime. The agent refuses with `MemoryCheckFailed` once such
failures have lasted 3 minutes, and at once for a definitive answer
such as a 401, 403 or 404.

The agent also implements **memory-pressure protection**: if
macOS reports critical memory pressure, the agent can evict the
lowest-priority running InferenceService, keep it stopped until
pressure returns to normal, and then start it again once available
memory has room for it. See the
[`Memory-pressure protection`](../memory-pressure-protection)
guide for tuning.

## ModelRouter integration

The Metal Agent's InferenceServices are first-class targets for
the `ModelRouter` CRD. Reference them by name like any other
local backend:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata: { name: hybrid-router }
spec:
  backends:
    - name: local-mac
      inferenceServiceRef: { name: phi-4-mini }   # the InferenceService above
      tier: local
      capabilities: [chat]
    - name: cloud-opus
      external:
        provider: anthropic
        model: claude-opus-4-7
        credentialsSecretRef: { name: anthropic-key }
      tier: cloud
  rules:
    - name: pii-stays-on-mac
      match: { dataClassification: [pii] }
      route: { backends: [local-mac] }
      failClosed: true
  defaultRoute: local-mac
```

The router-proxy pod (which the controller schedules in the
cluster, *not* on the Mac) calls the `phi-4-mini` Service, and so
the relay in front of the agent's ingress, when the rule resolves
to `local-mac`. From the router's
perspective the Mac-served backend is indistinguishable from a
container-served one — same `InferenceServiceRef` shape, same
fail-closed semantics, same per-rule timeout budgets.

See the [`ModelRouter concept doc`](../concepts/model-router) for
the full policy model.

## Cross-cluster fleet shape

Heterogeneous clusters are the strongest pattern: NVIDIA nodes in
a cloud for heavy workloads, Mac Studios on-prem for
low-latency / sensitive work, all managed by the same controller
with the same CRDs. The agent makes the Mac visible to the
controller exactly like a Linux node visible to a `Deployment`
reconciler — just with `accelerator: metal` instead of
`accelerator: cuda` on the `Model`.

Operationally:

- Put the Mac on the same VPN / Tailscale tailnet as your
  cluster's worker nodes.
- Set `--host-ip` to the Mac's address on that network.
- Each agent claims every `accelerator: metal` InferenceService in
  its `--namespace`. With more than one Mac on a cluster, give each
  agent `--inference-service-allowlist`, a comma-separated list of the
  InferenceService names it should serve, so two Macs do not both
  start the same model:

  ```xml
  <string>--inference-service-allowlist</string>
  <string>phi-4-mini,qwen-coder</string>
  ```

## MetalNode: the Mac as a cluster object

A `MetalNode` (short name `mn`) is one Mac, as the cluster sees it:
its hardware, the agent version, the address the cluster verified it
can reach, and whether the agent is alive. It is a custom resource,
not a Kubernetes `Node`, so nothing schedules pods onto it.

```bash
kubectl get mn
# NAME         HOST IP      MEMORY   CHIP           REACHABLE   VERSION   AGE
# mac-studio   100.64.0.2   128Gi    Apple M5 Max   True        0.11.0    3m
```

Three parties each write only what they know:

- **The admin** creates the MetalNode, a ServiceAccount for the Mac,
  and a `coordination.k8s.io` Lease for its heartbeat. `spec` names
  the ServiceAccount and the Lease, and can carry an optional
  `hostIP` (an address you know reaches the Mac; tried first and
  still verified) and `verifier` placement (`nodeSelector`,
  `tolerations`).
- **The agent** never writes the MetalNode. Started with
  `--metal-node <name>`, it renews the Lease every 15 seconds and
  puts what it knows in the Lease's `llmkube.ai/metal-node-report`
  annotation: version, chip, memory, GPU cores, Metal version, the
  ingress port, the ingress SPKI pin, the ingress certificate, and the
  addresses it thinks the cluster can reach it at, in order of
  preference. Before it reports, it reads its MetalNode's UID and makes
  its ingress certificate name it (see [The dial-back](#the-dial-back)).
- **The controller** is the only writer of MetalNode status. It
  copies the report into status, sets `AgentConnected` from the
  Lease's `renewTime`, and proves reachability with a dial-back
  before setting `Reachable` and `status.hostIP`.

### Running the agent with a MetalNode

Add the flag to the agent's launchd plist (or command line):

```xml
<string>--metal-node</string>
<string>mac-studio</string>
```

The agent renews the Lease with the same name as the MetalNode;
`--metal-node-lease <name>` picks a different one. It only ever gets
and updates that Lease, and gets the MetalNode (for its UID, at start
and every 5 minutes after). If the Lease does not exist, the agent logs
`Lease <namespace>/<name> not found; run llmkube metal token create`
at most once a minute and keeps serving. Without `--metal-node` the
agent makes no Lease call at all.

The candidate addresses follow the route to the API server (the
`server:` in the agent's kubeconfig): `--host-ip` first; for a
cluster on the same Mac (API server on loopback, as with kind on
Docker Desktop), `192.168.65.254`, then LAN, then tailnet; for a
cluster reached over a tailnet or VPN (`100.64.0.0/10`, or a route
through a `utun` interface), the Mac's address on that interface
first; otherwise the address the Mac's kernel sends from toward the
API server, then its other private addresses. The list is
recomputed at every renewal, so a network change reaches the cluster
within 15 seconds and triggers a new dial-back.

### The objects for each Mac

`llmkube metal token create <mac>` creates the MetalNode, its Lease,
the Mac's ServiceAccount (`llmkube-metal-<mac>`), the per-Mac Role and
RoleBinding (`llmkube-metal-<mac>`), the binding to the shared agent
Role (`llmkube-metal-<mac>-shared`) and a single-use join Secret
(`llmkube-metal-<mac>-join`). Every one carries the label
`inference.llmkube.dev/metal-node: <mac>`, and the command refuses to
take over an existing object of the same name without it. The
MetalNode and Lease it creates for `mac-studio` are equivalent to:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: MetalNode
metadata:
  name: mac-studio
  labels:
    inference.llmkube.dev/metal-node: mac-studio
spec:
  serviceAccountName: llmkube-metal-mac-studio
  leaseName: mac-studio
---
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: mac-studio
  labels:
    # The controller watches only Leases with this label. An unlabeled
    # Lease is still read, but only on the controller's 60 s resync.
    inference.llmkube.dev/metal-node: mac-studio
spec:
  holderIdentity: mac-studio
  leaseDurationSeconds: 45
```

The per-Mac Role grants `get` and `update` on the Mac's own Lease and
`get` on its own MetalNode, by `resourceNames`; the full set of
objects is in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md).

### The dial-back

`Reachable` means a pod inside the cluster completed a TLS handshake
with this Mac's ingress: a server presenting the certificate the agent
reported, which names this MetalNode, with the key the agent reported.
The controller runs it as a short-lived verifier pod in the
MetalNode's namespace, owned by the MetalNode, placed by
`spec.verifier`:

- It uses the operator's `--init-container-image` (curl), runs as a
  non-root user with a read-only root filesystem (its only writable
  path is a 64 KiB in-memory `emptyDir` for the certificate), no
  capabilities and no service account token, is killed after 60
  seconds, and stops at once when deleted (no termination grace
  period). It opts out of Istio and Linkerd sidecar injection
  (`sidecar.istio.io/inject: "false"` as label and annotation,
  `linkerd.io/inject: disabled`): a sidecar would keep the pod from
  finishing and send the probe through the mesh instead of straight to
  the Mac.
- The agent's ingress certificate names the MetalNode:
  `<metadata.uid>.metal-node.llmkube.invalid` (`.invalid` never
  resolves and no public CA issues for it). The agent reads the UID
  from its MetalNode and re-issues the certificate for the same key,
  so the SPKI pin, which the relay and the EndpointSlices carry, does
  not change. The certificate is saved beside the key in the agent's
  state directory.
- For each candidate it runs curl against
  `https://<uid>.metal-node.llmkube.invalid:<ingressPort>/health`,
  with `--resolve` sending that name to the candidate address,
  `--cacert` set to the certificate the agent reported, `--capath` set
  to an empty directory (so that certificate is the only trust anchor,
  even with a curl built with a CA directory) and
  `--pinnedpubkey sha256//<pin>`, ignoring any
  `.curlrc` and any proxy settings, so it tests the direct path. The
  ingress answers 400 to a request without its relay headers, which is
  enough: curl completes the handshake only with a server whose
  certificate is the reported one, names this MetalNode and carries
  the pinned key. Another server at a reported address (an API server,
  whose address and public key any client sees) presents its own
  certificate, which does not name the MetalNode, so it fails
  (`CertificateMismatch`) whatever key and address the agent reports.
- The controller checks the reported certificate before it dials: it
  must be for the pinned key and signed by that key (otherwise the
  report is invalid), and it must name this MetalNode (otherwise
  `Reachable=False`, reason `CertificateUnbound`, until the agent binds
  it). An agent that reports a pin but no certificate (any agent from
  before this check) is never dialed: `Reachable=False`, reason
  `AgentUpgradeRequired`. Upgrade the agent; until then the MetalNode
  is not Reachable.
- The certificate is checked for its validity period on the verifier
  node's clock. The agent backdates it by a day, so only a node clock
  more than a day behind the Mac's fails a freshly re-issued one. The
  agent also re-issues it (same key) whenever it is not valid on the
  Mac's own clock, so one issued while that clock ran fast is replaced
  within 5 minutes of the clock being fixed. Deleting
  `ingress-cert.pem` (keeping `ingress-key.pem`) makes the agent issue
  a fresh one at its next start, with the same pin.
- The image's curl must be 7.57 or later and accept a self-signed
  certificate as its own trust anchor (OpenSSL and LibreSSL do; see the
  [air-gapped guide](./air-gapped) for a custom image). Any other curl
  fails closed: `Reachable` stays False.
- What the check does not cover: a server that answers any requested
  name with a certificate it self-signs on the spot with one stable
  key (some TLS-intercepting proxies and development servers do) would
  pass. The agent could ask that server for the MetalNode's name and
  report the certificate it gets back, with its key. A challenge the
  ingress has to answer with its key would close this; the check does
  not have one yet.
- Per-candidate results are `Reachable`, `Timeout`, `Refused`,
  `PinMismatch`, `CertificateMismatch`, `NotAllowed` or `Error`. The
  first `Reachable` address in order becomes `status.hostIP`, which is
  set only while `Reachable` is True.

The check reruns when the pin, the certificate, the port, the
addresses it dials or the placement change, every 10 minutes, and
after a failure with a
backoff of 30 seconds growing to 5 minutes. Two rules hold whatever
the Mac reports: at most one verifier pod exists per MetalNode at a
time (a pod being deleted still counts), and two verifier starts are
at least 30 seconds apart. Rewording a candidate's source or changing
an address that is never dialed is not a change. Only the verifier
pod the controller created (recorded by UID in
`status.verification.podUID`) can report a result. When the pin or
port changes, or the verified address drops out of the list, the old
`Reachable=True` and `hostIP` are withdrawn at once (`Reachable` is
`Unknown`, reason `VerificationPending`) until the new check lands; a
new address offered beside the verified one leaves the result
standing, but no success stands for more than 13 minutes (the
10-minute recheck, the 2-minute verifier timeout and a minute of
slack) without a newer check finishing. A Mac whose report keeps
changing restarts the check each time, so without that bound an old
success, and its `hostIP`, would outlive the address it proved; past
it `Reachable` is `Unknown`, reason `VerificationPending`, and the
message names a verifier pod stuck terminating if one is what keeps
the check from starting. Times a verifier's node reports are kept
between the verifier's start and the moment the controller reads it,
so a node clock that is off does not change when a result expires or
when the next check runs. Put the verifier
where the relay pods for this Mac run, so it tests the path the relay
will use.

While `AgentConnected` is not True, nothing is dialed: `Reachable`
is `Unknown` (reason `ReportInvalid` when the report is what is wrong,
`AgentNotConnected` otherwise), `status.hostIP` is cleared, and any
verifier pod is deleted. When the Lease is missing or
its report is invalid, the agent's reported facts (`agentVersion`,
`capacity`, `ingressPort`, `ingressSPKI`) are cleared too; a stale but
valid report keeps them as the last known facts. `status.lastHeartbeatTime`
is accurate to about a minute: it advances when `AgentConnected`
changes or when it is more than 60 seconds behind the Lease, which
holds the exact time. A Lease update that only advances `renewTime`
on schedule does not wake the controller; its 60 s resync does.

### Security notes

- **The agent cannot mark itself reachable.** It has no write access
  to the MetalNode; only the controller sets `Reachable` and
  `hostIP`. Everything in the Lease report is treated as a claim:
  bounded, validated (a malformed report or pin, an ingress port below
  1024, or text with non-printable characters such as bidi overrides
  is `AgentConnected=False`, reason `ReportInvalid`), and verified
  where it can be. A `renewTime` more than 30 seconds in the future
  counts as stale.
- **The dial-back is bound to the Mac.** It completes TLS only with a
  server whose certificate names this MetalNode's UID and carries the
  SPKI pin the agent reported (checked with the same rule the relay
  uses), so the agent cannot get `Reachable=True` for another
  server's address by reporting that server's key. It copies no
  response body or curl error
  into status, and never passes an agent value through a shell:
  addresses must be IP literals and reach the pod as arguments.
- **Allowed CIDRs.** The verifier only dials candidates inside the
  controller's `--metal-node-allowed-cidrs` (default
  `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10`, which
  covers Docker Desktop's `192.168.65.254`). Anything else is marked
  `NotAllowed`. Loopback, link-local (where cloud metadata services
  live) and multicast addresses are never dialed, whatever the list
  says. A Mac still chooses which allowed addresses and which port
  (1024 or above) get a single TLS probe, at most once every 30
  seconds. Narrow the list to your Mac networks, and keep your pod
  and Service CIDRs out of it, if that matters in your cluster.
- **The dial-back results are visible to the Mac.** The Mac can read
  its own MetalNode, including the per-candidate results. They stay
  coarse on purpose (`Reachable`, `Timeout`, `Refused`, `PinMismatch`,
  `CertificateMismatch`, `NotAllowed`, `Error`), with no response
  bodies, curl errors or timings.
- **No Kubernetes version floor.** The MetalNode CRD uses no CEL
  rule that needs a newer Kubernetes than the rest of LLMKube;
  `spec.hostIP` is checked by the controller (`Reachable=False`,
  reason `InvalidSpec`, when it is not an IP address).

### Conditions

| Condition | Reason | Meaning |
|---|---|---|
| `AgentConnected` | `HeartbeatFresh` | The Lease was renewed within three lease durations. |
| `AgentConnected` | `HeartbeatStale` | The agent stopped renewing (or never started), or `renewTime` is more than 30 s in the future. |
| `AgentConnected` | `LeaseNotFound` | The Lease in `spec.leaseName` does not exist. |
| `AgentConnected` | `ReportInvalid` | The report annotation is missing, malformed, or carries an invalid pin, a port below 1024, non-printable text, or an ingress certificate that is not for the pinned key or not signed by it. |
| `Reachable` | `DialBackSucceeded` | A verifier reached `status.hostIP` and the server presented the agent's certificate for this MetalNode with the pinned key. |
| `Reachable` | `NoneReachable` | No candidate answered with the agent's certificate and pinned key; see `status.candidates`. |
| `Reachable` | `NoCandidates` | No reported address is inside the allowed CIDRs. |
| `Reachable` | `PinMissing` | The agent reports no ingress pin (legacy mode). |
| `Reachable` | `AgentUpgradeRequired` | The agent reports a pin but no ingress certificate, as agents from before the certificate check do; nothing is dialed until the agent is upgraded. |
| `Reachable` | `CertificateUnbound` | The agent's ingress certificate does not name this MetalNode's UID; the agent binds it once it can read its MetalNode (see the agent log). |
| `Reachable` | `VerifierFailed` | The verifier pod could not be created, failed, or never finished (check the controller log, `spec.verifier` placement and the image pull), or a pod this MetalNode does not own holds the verifier's name. |
| `Reachable` | `VerificationPending` | `Unknown`: the pin or port changed, the verified address is no longer offered, or the last success is more than 13 minutes old with no newer check finished, so the last result no longer applies; `hostIP` is empty until the new check lands. |
| `Reachable` | `AgentNotConnected` | `Unknown`: the agent is not connected, so nothing is dialed and `hostIP` is empty. |
| `Reachable` | `ReportInvalid` | `Unknown`: the agent's report on its Lease is invalid (see `AgentConnected`), so nothing is dialed and `hostIP` is empty. Fix the report, not the Mac's network. |
| `Reachable` | `InvalidSpec` | `spec.hostIP` is not an IP address; nothing is dialed until it is fixed. |

## Join a Mac

Joining a Mac takes two commands on two machines, and the Mac never
sees your cluster credentials:

1. **Where you have cluster access** (your laptop, a bastion), an
   admin creates the Mac's identity and a join string:

   ```bash
   llmkube metal token create mac-studio -n default
   # llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
   # valid for 30 minutes (until 2026-10-25 15:04 PDT), single use, Mac mac-studio in namespace default
   # CA pin: sha256:a5fee586b3b05aaff494d200d4c072e2e37126eaef344e2f295ad87479272655
   ```

   The first line is the whole command to paste on the Mac, alone on
   its line. Progress goes to stderr, so stdout copies cleanly;
   `-o json` prints `joinCommand`, `expiresAt`, `caPin`, `namespace`
   and `mac` for scripts.

2. **On the Mac**, with `llmkube` and `llmkube-metal-agent`
   installed, paste that line:

   ```bash
   llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
   # joining Mac mac-studio in namespace default at https://100.64.0.1:6443
   # CA pin: sha256:a5fee586b3b05aaff494d200d4c072e2e37126eaef344e2f295ad87479272655 (compare it with ...)
   # ...
   # Mac mac-studio joined: the cluster reaches it at 100.64.0.5:9443 (log: ~/Library/Logs/llmkube/metal-agent.log)
   ```

   It sets up and starts the agent with the Mac's own identity and
   returns once the cluster has verified it can reach the Mac. See
   [On the Mac: join and leave](#on-the-mac-join-and-leave).

The join string carries the API server URL, the cluster's CA
certificate, the CA's pin, the namespace, the Mac's name and a join
token. The Mac trusts only that CA (no system roots). Compare the
`CA pin` line with the pin `join` prints on the Mac: if they differ,
the string was altered on the way.

### What `token create` creates

Everything is in the namespace you pass with `-n` (default: your
kubeconfig context's namespace, else `default`), named after the Mac
(`<mac>` must be a DNS label, at most 63 characters), and labeled
`app.kubernetes.io/managed-by=llmkube-cli` and
`inference.llmkube.dev/metal-node=<mac>`:

| Object | Name | Notes |
|---|---|---|
| ServiceAccount | `llmkube-metal-<mac>` | `automountServiceAccountToken: false`. The Mac's identity. |
| Role | `llmkube-metal-<mac>` | The per-Mac rules below. |
| RoleBinding | `llmkube-metal-<mac>` | Binds the per-Mac Role to the ServiceAccount. |
| RoleBinding | `llmkube-metal-<mac>-shared` | Binds the shared `llmkube-metal-agent` Role to the ServiceAccount. |
| Lease | `<mac>` | The agent's heartbeat. `leaseDurationSeconds: 45`; the label lets the MetalNode controller watch it. |
| MetalNode | `<mac>` | `spec.serviceAccountName` and `spec.leaseName` point at the two above. |
| Secret | `llmkube-metal-<mac>-join` | Opaque and empty: it exists only so the join token can be bound to it. |

The per-Mac Role grants only the Mac's own objects, all by
`resourceNames`:

```yaml
rules:
  # Renew its own credential (and no one else's).
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["llmkube-metal-<mac>"]
    verbs: ["create"]
  # Read its own MetalNode. The agent never writes it.
  - apiGroups: ["inference.llmkube.dev"]
    resources: ["metalnodes"]
    resourceNames: ["<mac>"]
    verbs: ["get"]
  # Heartbeat and report.
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    resourceNames: ["<mac>"]
    verbs: ["get", "update"]
  # Delete its join Secret once, which kills the join token.
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["llmkube-metal-<mac>-join"]
    verbs: ["delete"]
```

Everything else the agent does comes from the shared
`llmkube-metal-agent` Role, the same audited rules as
[`deployment/macos/metal-agent-rbac.yaml`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/metal-agent-rbac.yaml).
If the namespace has no such Role, `token create` creates it from a
copy of that file built into the CLI. If one exists with different
rules (the chart's `metalAgent.rbac.secretNames` pins the Secret rule
to names, for example), it is left as it is and `token create` prints
a warning.

`token create` is safe to re-run. It updates its own objects, keeps
the Lease (the agent's heartbeat) and any `hostIP` or `verifier` you
set on the MetalNode, and refuses to take over an object with one of
these names that is not labeled for this Mac (or is labeled for
another Mac). It checks all of them before it writes anything, so a
refusal leaves the cluster as it was. If you created the
objects by hand as in [Creating the objects](#creating-the-objects),
label them (`kubectl label ... inference.llmkube.dev/metal-node=<mac>`)
or delete them first.

### The join token

- **Short-lived.** It works for 30 minutes (`--ttl`, from `10m`, the
  shortest token the API server issues, to `24h`). The printed expiry
  is the API server's.
- **Single use.** The token is bound to the join Secret. `join`
  trades it for the agent's own credential and then deletes the
  Secret, which invalidates the token even inside its 30 minutes
  (within ~10 seconds: the API server caches a token it just
  accepted).
- **A credential until it is used.** Until the string is redeemed or
  expires, whoever holds it can act as the Mac, including minting the
  Mac's own long-lived tokens. Treat it like a password: paste it
  only on the Mac.
- **One unused string per Mac.** Running `token create` again for the
  same Mac recreates the Secret, so a previous string that was never
  used stops working. It does **not** revoke a string that was
  already redeemed: its holder may already have minted tokens of
  their own. If a string may have leaked, run
  `llmkube metal remove <mac>` (that revokes every token for the Mac
within ~10 seconds)
  and then `token create` again.
- **Checked before anything is created.** The command checks every
  field of the join string, then reads the API
  server's certificate and verifies it against your kubeconfig's CA.
  It refuses a kubeconfig with `insecure-skip-tls-verify` or no CA,
  since the Mac would have nothing to trust. When the server address
  is not in the certificate (a tailnet address, say), it sets the TLS
  server name to `kubernetes` if the certificate covers that, or
  stops and lists the names the certificate does cover.

| Flag | Default | Meaning |
|---|---|---|
| `-n`, `--namespace` | context namespace, else `default` | Namespace for the Mac's objects. |
| `--ttl` | `30m` | How long the join string works (`10m` to `24h`). |
| `--server` | the context's server | API server URL the Mac connects to, for when the Mac reaches it by another address. This machine must reach it too, to check its certificate, unless `--assume-same-server`. |
| `--assume-same-server` | off | When this machine cannot reach `--server` (an address only the Mac can reach), check the certificate the context's server presents instead. Nothing then proves `--server` is the same API server; the Mac still verifies it against the CA when it joins. |
| `--tls-server-name` | automatic | Name the Mac verifies the certificate against. It must be in the certificate. |
| `-o`, `--output` | `text` | `text` or `json`. |

### On the Mac: join and leave

`llmkube metal join <join-string>` runs on the Mac (it refuses to run
elsewhere). In order:

1. Decodes the string and checks the CA against its pin, then prints
   the pin. Compare it with the one `token create` printed.
2. Finds `llmkube-metal-agent`: `--agent-binary`, else on `PATH`,
   else next to `llmkube` (a release archive, a Homebrew prefix). Finds
   `llama-server`, which the agent cannot start without:
   `--llama-server`, else `/opt/homebrew/bin` or `/usr/local/bin`, else
   on `PATH`. Either one missing stops join before it changes anything,
   so the join string still works after you install it. So does a
   `com.llmkube.metal-agent` LaunchAgent that `join` did not write
   (`make install-metal-agent`, a playbook): it runs the agent with the
   Mac's own arguments, so `join` refuses to replace it without
   `--force`.
3. With the join token, which trusts only the string's CA, mints the
   agent's own token for the Mac's ServiceAccount (`--token-lifetime`,
   7 days by default).
4. Writes that token and a kubeconfig that reads it through
   `tokenFile`, both `0600`, in
   `~/Library/Application Support/llmkube/metal-agent/` (`0700`; the
   agent's state directory, which also holds its ingress identity).
5. Checks the new credentials can read the Mac's MetalNode, then
   deletes the join Secret: the join string stops working (within
   ~10 seconds, as above). A failure before this step leaves the
   string usable, so you can fix the cause and run `join` again.
6. Writes `~/Library/LaunchAgents/com.llmkube.metal-agent.plist`
   and starts it with `launchctl bootstrap`. A plist `join` did not
   write, replaced with `--force`, is kept as
   `com.llmkube.metal-agent.plist.bak` in the state directory, not in
   `LaunchAgents`, where launchd could load it at login; a later `join`
   does not overwrite that copy. The agent runs with `--kubeconfig`,
   `--namespace`, `--metal-node` and `--renew-service-account-token`,
   and logs to `~/Library/Logs/llmkube/metal-agent.log`. If
   `launchctl bootstrap` fails, the string is already used, but the
   credentials are in place: the error prints the command that starts
   the agent once the cause is fixed.
7. Waits up to `--wait` (3 minutes; `0` does not wait) for the
   MetalNode to report `Reachable`, printing each change of
   `AgentConnected` and `Reachable`. Only status for the agent this
   `join` started counts: its Lease renewed after the old agent was
   stopped, the controller caught up with the MetalNode's generation,
   and `Reachable` verified for the ingress key and an address the new
   agent reports. On a re-join, the replaced agent's `Reachable=True`
   stays for minutes and does not end the wait. If the wait runs out,
   `join` fails with the last conditions it saw and leaves the agent
   installed and retrying.

| Flag | Default | Meaning |
|---|---|---|
| `--agent-binary` | on `PATH`, else next to `llmkube` | Path to `llmkube-metal-agent`. |
| `--llama-server` | Homebrew's, else on `PATH` | Path to `llama-server`; join passes the path it found to the agent. |
| `--token-lifetime` | `168h` (7 days) | Lifetime of the agent's token (at least `10m`). |
| `--wait` | `3m` | How long to wait for `Reachable`; `0` returns once the agent is started. |
| `--force` | off | Replace a `com.llmkube.metal-agent` LaunchAgent that `join` did not write. |

**The agent renews its own token.** With
`--renew-service-account-token`, the agent requests a new token for
its own ServiceAccount at half the current one's lifetime and
replaces the token file; client-go re-reads the file, so nothing
restarts. The API server may issue a shorter token than asked for
(`--service-account-max-token-expiration`); `join` warns, and renewal
follows the token's real lifetime. A Mac that is off for longer than
the token's lifetime cannot renew it: run `token create` and `join`
again. Running `join` again on a Mac replaces its setup.

`llmkube metal leave` undoes `join` on the Mac: it stops the
LaunchAgent and removes its plist, the kubeconfig and the token. Engines
outlive the agent, so once the agent has exited, `leave` runs its
`stop-engines` first, which stops the engines the agent started and
withdraws their endpoints. If `stop-engines` fails, `leave` removes
nothing; check its output and run `leave` again. An agent binary from
before `stop-engines` stops its engines when it exits, so `leave` skips
the step for it. The ingress identity and the model store stay. A
LaunchAgent `join` did not write is left running, with its engines. If `join --force` replaced one, `leave`
prints the commands that put it back. It does not revoke
anything; the token stays valid until it expires, so also run
`llmkube metal remove` where you have cluster access.

### Revoking a Mac

```bash
llmkube metal remove mac-studio -n default
```

`remove` deletes the Mac's ServiceAccount first and waits until it is
gone. That is the revocation: every token the Mac holds stops working
within ~10 seconds, including the ones minted from the join string or
by the agent itself (the API server keeps accepting a token it
accepted in the last ~10 seconds for up to ~10 seconds more). Then it
deletes the per-Mac Role, both RoleBindings, the Lease, the join
Secret and the MetalNode, and prints each one it removed. It never
deletes the shared `llmkube-metal-agent` Role, and objects that are
already gone are skipped, so running it twice is fine. It does not
touch the Mac itself; run `llmkube metal leave` there.

`remove` deletes only objects labeled
`inference.llmkube.dev/metal-node=<mac>`. Anything else with one of
the Mac's names (another application's Lease, objects labeled for
another Mac, a MetalNode applied by hand) is left in place and listed
as `skipped`. If the ServiceAccount is among them, nothing was
revoked and the command fails. For a Mac whose objects you applied by
hand without the label, `--include-unlabeled` also deletes unlabeled
objects with the Mac's names, with a warning for each; objects
labeled for another Mac are never deleted.

### Limit the agent's writes (Kubernetes 1.30+)

The agent's Role lets it create, update and delete any Service and
EndpointSlice in its namespace, because RBAC cannot scope a verb to a label.
A ValidatingAdmissionPolicy narrows that to the objects the agent manages:
for every ServiceAccount named `llmkube-metal-<name>`, a write must leave the
label `llmkube.ai/managed-by=metal-agent` on a Service, EndpointSlice or
Endpoints object, and an update or delete must find it there. An update of
the Service an InferenceService controls is also allowed, so the agent can
take it over after a CUDA to Metal switch or a relay hand-back. The
`llmkube-metal-` ServiceAccount prefix is reserved for Mac agents.

The Helm chart installs the policy by default
(`metalAgent.writePolicy.enabled`). Without Helm, a cluster admin applies
[`deployment/macos/metal-agent-write-policy.yaml`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/metal-agent-write-policy.yaml)
once; do not apply both. On Kubernetes older than 1.30 only the Role applies.
See
["Limiting the agent's writes"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#limiting-the-agents-writes-admission-policy-kubernetes-130)
for what it does and does not stop.

## Optional: Apple Silicon power metrics

For [InferCost](https://github.com/defilantech/infercost) (LLMKube's
companion FinOps project) per-token cost attribution on Apple
Silicon, the agent can publish CPU / GPU / ANE / Combined power
gauges sourced from `powermetrics`. This is **disabled by default**
because `powermetrics` requires root.

Enable in three steps:

1. Install the bundled NOPASSWD sudoers fragment, which pins both
   the binary path and the arguments (no wildcards), so it matches
   only the one command line the agent runs:

   ```bash
   make install-powermetrics-sudo
   ```

   The sampling interval is part of the pinned arguments. If you
   run the agent with a non-default `--apple-power-interval`, pass
   the same value in whole milliseconds (minimum 100), for example
   `make install-powermetrics-sudo APPLE_POWER_INTERVAL_MS=2000`
   for `--apple-power-interval=2s`. When the installed rule does
   not match, sudo refuses the agent's command and the agent logs
   `apple-power: powermetrics exited unexpectedly` with a `remedy`
   field naming the command to run. Upgrading the agent does not
   change an installed rule; re-run the install after upgrading.

2. Add `--apple-power-enabled` to the launchd plist's
   `ProgramArguments` array.

3. Reload the agent.

The four gauges exposed:
`llmkube_metal_agent_apple_power_combined_watts`,
`llmkube_metal_agent_apple_power_gpu_watts`,
`llmkube_metal_agent_apple_power_cpu_watts`,
`llmkube_metal_agent_apple_power_ane_watts`.

See the
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#apple-silicon-power-metrics-for-infercost)
for the full sudoers setup and a manual install path that lets you
inspect each step before running it.

## Troubleshooting

**Agent process not running after install**
Check `~/Library/Logs/llmkube/metal-agent.log` (the
`StandardOutPath`/`StandardErrorPath` configured in the bundled
launchd plist) for the first-launch error. Most common cause:
`llama-server` not on PATH or at the configured `--llama-server`
path.

**Pods can't reach the model (remote cluster)**
Confirm `--host-ip` is set in the plist and points at an address
reachable from your cluster's worker nodes on the ingress port
(`--ingress-port`, default 9443). The engine port is not reachable
from the network by design.

```bash
# From a worker node:
ping <your-mac-ip>
nc -vz <your-mac-ip> 9443
```

Then check the agent's registration and the relay:

```bash
kubectl get endpointslice <inferenceservice-name>-agent -o yaml
# expect: your Mac's --host-ip, port 9443, and the
# llmkube.ai/agent-ingress-spki annotation
kubectl get pods -l inference.llmkube.dev/service=<inferenceservice-name>
kubectl logs deploy/<inferenceservice-name>-relay
kubectl describe inferenceservice <inferenceservice-name>
# look for RelayNotAdopted, InvalidAgentIngressPin, RelayReconcileFailed, RelaySecretNotManaged
```

**InferenceService stuck in `InsufficientMemory`**
The agent's pre-flight estimator says the model won't fit. Either
shrink the model (use a smaller quantization), reduce the context
size in the `InferenceService` spec, or raise
`--memory-fraction`. If the Mac is the only Mac in the cluster and
this is a dedicated inference machine, `0.9` is reasonable.

**InferenceService stuck in `MemoryCheckFailed`**
The agent could not size the model at all, so it refused to start
it. `status.schedulingMessage` lists every source it tried (local
file, Model `status.size`, HEAD probe of the source URL). The most
common causes are a gated Hugging Face repo rejecting the HEAD
probe (401), a source URL that does not return `Content-Length`, or
a source that stayed unreachable for the 3 minutes the agent retries
a failed probe quietly.
Fix the source so it can be probed, wait for the Model controller
to populate `status.size`, or start the agent with
`--memory-check-mode warn` to admit unsized models at your own
risk.

**Model download blocked by the SSRF guard, or `ModelDigestMismatch`**
The Model's source resolves to a private, loopback or link-local
address, which the agent refuses to fetch by default; the event
names `--allowed-download-hosts`. Or `spec.sha256` didn't match what
was downloaded, the bad file was deleted, and the InferenceService is
refused with reason `ModelDigestMismatch`. Both are covered in
"Security model" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#security-model).

**InferenceService refused with `ModelDownloadTooLarge`**
The Model's download went over the agent's `--max-download-size` cap
(default: the Mac's total memory, since a model file larger than RAM
cannot be wired into memory). A source that declared a larger size was
refused before anything was written; a body that streamed past the cap
was stopped and its partial file removed. Fix `spec.source` if it is
wrong, or, if the model really is that large, raise
`--max-download-size` (`0` disables the cap) and restart the agent. See
"`--max-download-size` flag (download cap)" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#--max-download-size-flag-download-cap).

**InferenceService refused with `SourceSecretNotAllowed`**
The Model's `spec.sourceSecretRef` names a Secret the agent will not
read: it is not labeled
`inference.llmkube.dev/model-source-credentials=true`, it does not
exist in the Model's namespace, the agent's RBAC does not let it read
it (a Secret outside `metalAgent.rbac.secretNames`), or it is the
relay token Secret `llmkube-metal-relay`. The message says which and
gives the `kubectl label` command; label, create, or allow the Secret
and the next poll starts the service. Keep
credentials out of the relay Secret. See
["Model-source credentials Secrets"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#model-source-credentials-secrets-sourcesecretref).

**Login Items lists the agent as "Defilan Technologies LLC"**
System Settings > General > Login Items & Extensions lists a release
agent under the name of the company that signs LLMKube's releases, not
as "LLMKube". macOS names a background item after the app it belongs
to. The agent is a plain executable that launchd starts, with no app
around it, so macOS uses the developer name on its Developer ID
certificate instead, and every agent the project signs shares that
one row. An agent you built yourself (`make install-metal-agent`) is
not Developer ID signed and shows under its executable name. After an
upgrade from a build like that, the old row can stay under that name
until macOS refreshes its records. Leave the toggle on: turning it off
stops macOS from starting the agent. To confirm the row is LLMKube's:

```bash
codesign -dv --verbose=4 "$(launchctl print gui/$(id -u)/com.llmkube.metal-agent | sed -n 's/^[[:space:]]*program = //p')" 2>&1 | grep -E '^(Identifier|Authority|TeamIdentifier)='
# Identifier=dev.llmkube.metal-agent
# Authority=Developer ID Application: Defilan Technologies LLC (SSJ3D786N9)
# Authority=Developer ID Certification Authority
# Authority=Apple Root CA
# TeamIdentifier=SSJ3D786N9
```

**macOS firewall prompt on first run**
The Metal Agent listens on `127.0.0.1:9090` for its own
health/metrics and serves its TLS ingress on `--ingress-port`
(default 9443) for inbound inference; engines listen on
`127.0.0.1` only. macOS will prompt to allow incoming connections
on first run. Allow them for the ingress port.

**Agent log shows `referenced model lookup failed; keeping the inference service's previous state`**
The agent could not read the Model an `InferenceService` references
(an API server timeout, throttling, a control-plane restart, or a
lost `get` permission on `models`). It leaves the service as it
was, so a running model keeps serving, and retries on every poll
(every 5 seconds). While the lookup fails, changes to the
`InferenceService` (`replicas: 0`, `suspend`, flag changes) do not
take effect; deleting the `InferenceService`, or restoring the
agent's access, does. The warning is logged once when the lookup
starts failing, and `referenced model lookup recovered` follows when
it succeeds again; the
`llmkube_metal_agent_watch_model_lookup_failing_services` gauge
counts the services in this state. If it stays above zero, check the
agent's RBAC and its connection to the API server. Of the possible
lookup results, only a deleted Model, or one that is no longer
`accelerator: metal`, stops the service.

**Agent log shows `replicas=0; stopping process` unexpectedly**
A controller-side reconcile saw `spec.replicas=0` on the
`InferenceService`. Check whether something scaled it down
(another operator, a Helm upgrade reverting your spec, an
operator-managed argocd app pulling a stale value).

## Engine environment

Engines the agent starts (llama-server, mlx-server, vllm-swift,
TensorFold, oMLX) do not inherit the agent's whole environment. They get
`PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `LANG`, `TZ`, every `LC_*`,
`HF_HOME` and `HF_HUB_CACHE`, plus the variables the agent sets for the
runtime (`GGML_METAL_ENABLE` and `GGML_METAL_PATH_RESOURCES` for
llama-server, `TENSORFOLD_NO_UPDATE_CHECK` for TensorFold). Credentials,
`KUBECONFIG`, proxies and other overrides in the agent's launchd
environment stay with the agent.

To pass a variable through, list its exact name in
`--engine-env-passthrough`:

```xml
    <string>--engine-env-passthrough</string>
    <string>HTTPS_PROXY,NO_PROXY,MLX_MAX_OPS_PER_BUFFER</string>
```

An invalid name stops the agent at startup. See
["Engine environment" in `deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#engine-environment---engine-env-passthrough)
for the details.

## Upgrading

Upgrade the operator first, then the agent on each Mac:

```bash
helm repo update
helm upgrade llmkube llmkube/llmkube -n llmkube-system
```

On a Mac that installed the agent with Homebrew, upgrade the cask and
restart the agent, which keeps running the old binary until it
restarts:

```bash
brew upgrade --cask llmkube-metal-agent
launchctl kickstart -k gui/$(id -u)/com.llmkube.metal-agent
```

On a Mac that built from source, check out the matching release in your checkout, unload
the agent, and reinstall it (`make install-metal-agent` alone keeps
running the plist launchd already loaded):

```bash
cd /path/to/LLMKube-checkout
git fetch --tags
git checkout v$(helm show chart llmkube/llmkube | awk '/^appVersion:/{print $2}')
launchctl bootout gui/$(id -u)/com.llmkube.metal-agent
while launchctl print gui/$(id -u)/com.llmkube.metal-agent >/dev/null 2>&1; do sleep 1; done
make install-metal-agent METAL_KUBECONFIG=$HOME/.kube/llmkube-metal-agent.yaml
launchctl print gui/$(id -u)/com.llmkube.metal-agent | grep 'state = running'
```

If you installed the agent without a dedicated kubeconfig, drop the
`METAL_KUBECONFIG=...` argument.

The reinstall rewrites the launchd plist from the template, so re-add
any flags you set by hand (`--host-ip`, `--memory-fraction` and so
on). Engines no longer inherit variables outside the
[engine environment](#engine-environment) allowlist; add any an engine
needs to `--engine-env-passthrough`. Engines adopted across the upgrade
keep their old environment until `stop-engines` or their next restart. Before upgrading across releases, read "Upgrading" and
"Upgrading to 0.10.1" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#upgrading)
and the
[upgrade and rollback runbook](https://github.com/defilantech/LLMKube/blob/main/docs/operations/runbooks/upgrade-rollback.md):
some releases change the plist or refuse an old configuration until
it is fixed. Before upgrading to the release that checks
`sourceSecretRef` Secrets, label every Secret your Models name with
`inference.llmkube.dev/model-source-credentials=true`, or run the agent
with the deprecated `--allow-unlabeled-source-secrets` for one release;
see ["Upgrade note: label existing credentials Secrets"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#upgrade-note-label-existing-credentials-secrets).
A new agent binary can also lose macOS Local Network
access; see
["LAN download fails with `no route to host` after an upgrade"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#lan-download-fails-with-no-route-to-host-after-an-upgrade).

## Uninstall

With Homebrew:

```bash
brew uninstall --zap --cask llmkube-metal-agent
```

`--zap` also stops the agent and removes its LaunchAgent, logs and
state. Without it, Homebrew removes only the binary and leaves the
LaunchAgent loaded: Homebrew runs a cask's uninstall steps on every
`brew upgrade` as well, so the cask leaves the LaunchAgent to zap.

From a source build:

```bash
cd /path/to/LLMKube-checkout
make uninstall-metal-agent
```

That tears down the launchd service, removes the binary from
`/usr/local/bin`, and deletes the plist. Model weights downloaded
into the agent's `--model-store` path stay on disk (the agent
doesn't clean those up; remove manually if needed).

## Reference

- [`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md) — full reference including manual install, launchd plist tuning, Prometheus metrics enumeration, sudoers fragment internals
- [`Memory-pressure protection`](../memory-pressure-protection) — eviction tuning and the InferenceService priority field
- [`Model Router`](../concepts/model-router) — policy-aware routing layer above Metal-served InferenceServices
- [`Air-gapped install`](./air-gapped) — combining Metal serving with offline / private-registry installs

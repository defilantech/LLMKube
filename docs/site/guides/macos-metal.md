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
  Metal support).
- **Xcode Command Line Tools** (`xcode-select --install`, for `git`
  and `make`) and **Go 1.26.10 or newer** (the version `go.mod` pins),
  to build the agent in Step 3.

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

Clone the operator repo on your Mac and check out the release that
matches the operator Helm installed:

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
`llmkube-metal-agent stop-engines` after the `bootout`. Before rolling the
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

## Optional: Apple Silicon power metrics

For [InferCost](https://github.com/defilantech/infercost) (LLMKube's
companion FinOps project) per-token cost attribution on Apple
Silicon, the agent can publish CPU / GPU / ANE / Combined power
gauges sourced from `powermetrics`. This is **disabled by default**
because `powermetrics` requires root.

Enable in three steps:

1. Install the bundled NOPASSWD sudoers fragment, which pins both
   the binary path and the argument vector so the grant is the
   narrowest possible:

   ```bash
   make install-powermetrics-sudo
   ```

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
probe (401) or a source URL that does not return `Content-Length`.
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

## Upgrading

Upgrade the operator first, then the agent on each Mac:

```bash
helm repo update
helm upgrade llmkube llmkube/llmkube -n llmkube-system
```

On each Mac, check out the matching release in your checkout, unload
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
on). Before upgrading across releases, read "Upgrading" and
"Upgrading to 0.10.1" in
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#upgrading)
and the
[upgrade and rollback runbook](https://github.com/defilantech/LLMKube/blob/main/docs/operations/runbooks/upgrade-rollback.md):
some releases change the plist or refuse an old configuration until
it is fixed. A new agent binary can also lose macOS Local Network
access; see
["LAN download fails with `no route to host` after an upgrade"](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#lan-download-fails-with-no-route-to-host-after-an-upgrade).

## Uninstall

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

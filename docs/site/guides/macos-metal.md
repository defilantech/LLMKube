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
with full Metal GPU access, and registers endpoints back into the
cluster so any pod can route traffic to your Mac over LAN /
Tailscale / WireGuard.

This guide gets you from a fresh Apple Silicon machine to a
running Metal-accelerated InferenceService in about ten minutes.

## Prerequisites

- **Apple Silicon Mac** (M1 / M2 / M3 / M4 / M5). Intel Macs with
  Metal 2+ work but performance is materially worse.
- **Access to a Kubernetes cluster** — either remote (recommended;
  most production patterns put the Mac on the LAN as an inference
  node) or local (minikube / kind on Docker Desktop).
- **kubectl** configured against your cluster.
- **Homebrew** (or any equivalent way to install `llama.cpp` with
  Metal support).

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

Clone the operator repo on your Mac and run the bundled installer:

```bash
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
make install-metal-agent
```

This builds the agent binary, installs to
`/usr/local/bin/llmkube-metal-agent`, drops a launchd plist into
`~/Library/LaunchAgents/`, and starts the service. On a fresh Mac
the whole thing takes about twenty seconds.

If you need to install manually (different binary path, different
launch system), see the
[`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md)
for the full plist and launchctl commands.

### Verify the agent is running

```bash
launchctl list | grep llmkube
# expected: <PID>   0   com.llmkube.metal-agent

curl -s http://localhost:9090/healthz
# expected: {"status":"ok"}

tail -f /tmp/llmkube-metal-agent.log
# leave this tab open; we'll watch it pick up the first InferenceService
```

## Step 4: Remote cluster setup

If your Kubernetes cluster runs on a different machine (a Linux
server or cloud cluster, as opposed to local kind / minikube), the
agent needs to register your Mac's reachable IP so cluster pods
can route to `llama-server` on your Mac.

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

Reload:

```bash
launchctl unload ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
launchctl load ~/Library/LaunchAgents/com.llmkube.metal-agent.plist
```

Without `--host-ip` the agent registers `localhost` as the
endpoint, which only works when Kubernetes lives on the same Mac
(local minikube or Docker Desktop kind).

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

The agent's log should show:

```
"msg":"starting inference service","name":"phi-4-mini"
"msg":"registered endpoint","hostIP":"192.168.1.50","port":<allocated>
"msg":"started inference service","name":"phi-4-mini","pid":<llama-server-pid>
```

### Find the endpoint

The metal-agent picks the port `llama-server` listens on at spawn
time and registers it as the Service's Endpoint. Unless you started
the agent with `--llama-server-port <N>`, the port is not 8080 — it
is allocated from the ephemeral range and changes across restarts.
Always read the endpoint from the cluster rather than assuming a port:

```bash
kubectl get endpoints phi-4-mini \
  -o jsonpath='{.subsets[0].addresses[0].ip}:{.subsets[0].ports[0].port}{"\n"}'
# example: 192.168.1.50:63344
```

The IP is your Mac's reachable address (LAN, Tailscale, etc., set via
`--host-ip` in Step 4). The port is whatever the agent allocated. The
two together are how every client — including pods inside the cluster
— reaches the model.

### Query the model

From the Mac that runs metal-agent, `localhost` works because
`llama-server` is bound to `0.0.0.0` on the host. Read the port from
the cluster and curl it:

```bash
PORT=$(kubectl get endpoints phi-4-mini -o jsonpath='{.subsets[0].ports[0].port}')
curl -sS "http://localhost:${PORT}/v1/chat/completions" \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

If your shell or paste medium strips backslash line-continuations,
the equivalent one-liner is safer to copy:

```bash
curl -sS "http://localhost:${PORT}/v1/chat/completions" -H 'content-type: application/json' -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
```

### Reaching the service from elsewhere

The InferenceService Service on the metal path is selector-less by
design: the metal-agent registers the host as the Endpoint, so the
Service has no Pods to target. That means
`kubectl port-forward svc/phi-4-mini ...` returns
`error: cannot attach to *v1.Service: ... Service is defined without
a selector` and cannot be used here. Two supported ways to reach the
service from a machine that is not the Mac:

1. **Hit the host directly.** Use the address `kubectl get endpoints`
   printed above. From any client on the same network:

   ```bash
   curl -sS "http://192.168.1.50:63344/v1/chat/completions" \
     -H 'content-type: application/json' \
     -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"hi"}]}'
   ```

   Substitute your own IP and port. This is the same address that
   in-cluster pods route to via the Service's ClusterIP, so a NodePort
   is not required for LAN clients.
2. **Pin the agent's port + set `spec.endpoint.type: NodePort`** if
   you want a stable, externally-advertised port that survives agent
   restarts. Start metal-agent with `--llama-server-port 8080` (or any
   fixed value you choose) and set the NodePort on the InferenceService.

## Memory budgets

The agent estimates each model's memory cost (weights + KV cache +
overhead) before spawning `llama-server`. If the model won't fit
in the configured budget, the agent refuses to start it and marks
the InferenceService with `status.schedulingStatus: InsufficientMemory`.

Defaults are tuned by total system RAM:

| Total RAM | Default fraction | Budget |
|---|---|---|
| 16 GB | 67% | ~10.7 GB |
| 36 GB | 67% | ~24.1 GB |
| 48 GB | 75% | 36 GB |
| 64 GB | 75% | 48 GB |
| 128 GB | 90% | 115 GB |

Override the fraction with `--memory-fraction 0.9` for a dedicated
inference machine, or `0.5` if the Mac is also your daily-driver
workstation. Add the flag to the launchd plist's `ProgramArguments`
the same way as `--host-ip`.

The agent also implements **memory-pressure protection**: if
macOS reports critical memory pressure, the agent can evict the
lowest-priority running InferenceService and refuse to spawn new
ones until pressure normalizes. See the
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
cluster, *not* on the Mac) dials the agent-registered endpoint
when the rule resolves to `local-mac`. From the router's
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
- The controller routes all `accelerator: metal` InferenceServices
  to whatever agent is registered for that endpoint.

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
Check `/tmp/llmkube-metal-agent.log` (the
`StandardOutPath`/`StandardErrorPath` configured in the bundled
launchd plist) for the first-launch error. Most common cause:
`llama-server` not on PATH or at the configured `--llama-server`
path.

**Pods can't reach llama-server (remote cluster)**
The agent registered `localhost`. Confirm `--host-ip` is set in
the plist and points at an address reachable from your cluster's
worker nodes:

```bash
# From a worker node:
ping <your-mac-ip>
curl http://<your-mac-ip>:<allocated-port>/v1/models
```

If those work but routing through the cluster Service fails, check
the registered Endpoints object:

```bash
kubectl get endpoints <inferenceservice-name>
# expect: subsets[0].addresses[0].ip = your Mac's --host-ip
```

**InferenceService stuck in `InsufficientMemory`**
The agent's pre-flight estimator says the model won't fit. Either
shrink the model (use a smaller quantization), reduce the context
size in the `InferenceService` spec, or raise
`--memory-fraction`. If the Mac is the only Mac in the cluster and
this is a dedicated inference machine, `0.9` is reasonable.

**macOS firewall prompt on first run**
The Metal Agent listens on `127.0.0.1:9090` for its own
health/metrics, and `llama-server` listens on an allocated port
for inbound inference. macOS will prompt to allow incoming
connections on first run. Allow them.

**Agent log shows `replicas=0; stopping process` unexpectedly**
A controller-side reconcile saw `spec.replicas=0` on the
`InferenceService`. Check whether something scaled it down
(another operator, a Helm upgrade reverting your spec, an
operator-managed argocd app pulling a stale value).

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

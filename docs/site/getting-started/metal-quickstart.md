---
title: "Quickstart: a cluster and the Metal agent on one Mac"
description: Run a Kubernetes cluster with Apple's container tool and the LLMKube Metal agent on the same Apple Silicon Mac, join the Mac to the cluster, and serve a small model with Metal in a few commands.
---

# Quickstart: a cluster and the Metal agent on one Mac

This quickstart runs a local Kubernetes cluster with Apple's
[`container`](https://github.com/apple/container) tool and the LLMKube Metal
agent on the same Mac, joins the Mac to that cluster, and serves a small model
with Metal. The cluster's node is a lightweight Linux VM with no GPU, so the
agent runs `llama-server` natively on the Mac, where Metal is. Engines listen
on `127.0.0.1` only, the cluster reaches them through the agent's
authenticated TLS ingress and an in-cluster relay, and clients on the Mac use
the agent's client proxy on `127.0.0.1:9999`.

Apple's `container` supports macOS 26 and later. On macOS 13 to 15, run the
cluster with minikube instead: see [On macOS 13 to 15](#on-macos-13-to-15).

For a cluster on another machine, the network requirements, Local Network
access, running a Mac as a dedicated host, upgrades and troubleshooting, follow
the [macOS Metal Agent guide](/docs/guides/macos-metal). The complete agent
reference is [`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md),
which the release archive also carries under `deployment/macos/`.

## What you need

- An Apple Silicon Mac with 16 GB of memory or more, on macOS 26 or later.
- To be logged in at the Mac's screen: the agent is a LaunchAgent and runs only
  in your login session.
- Ports 9443 and 9090 free on the Mac (the agent's ingress, and its health
  and metrics on `127.0.0.1`).
- Homebrew.

```bash
brew install container kubectl helm
container system start --enable-kernel-install
```

`container system start` starts Apple's container services. On the first run
it needs a Linux kernel for the VMs, and `--enable-kernel-install` installs
the default one instead of asking.

## 1. Create the cluster and install the operator

```bash
container cluster create --name llmkube

helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm install llmkube llmkube/llmkube -n llmkube-system --create-namespace \
  --set modelCache.mode=perService
kubectl wait --for=condition=ready pod -l control-plane=controller-manager \
  -n llmkube-system --timeout=120s
```

`container cluster create` runs a one-node cluster in about a minute, adds it
to `~/.kube/config` as the context `llmkube` and switches `kubectl` to it. The
node gets a quarter of the Mac's CPUs and memory by default, which is plenty:
the model runs on the Mac, not in the cluster.

The cluster has no default StorageClass, and the chart's default model cache
(`modelCache.mode=shared`) needs one: without the flag, `helm install` stops
with "LLMKube shared model cache requires a default StorageClass". The Metal
agent keeps models on the Mac, so the per-service mode loses nothing here.

## 2. Install the agent and the CLI

```bash
brew install defilantech/tap/llmkube defilantech/tap/llmkube-metal-agent
```

This installs the signed `llmkube-metal-agent`, the `llmkube` CLI and
`llama.cpp`. Name both: the agent cask does not depend on the CLI formula. The
cask does not start the agent; joining does. Homebrew installs the latest
release, and `helm repo update` above makes the operator the latest too; check
with `llmkube-metal-agent --version` and `helm list -n llmkube-system`.

## 3. Join the Mac

The agent serves one namespace. Give it one of its own: the joined Mac can
read Secrets in that namespace by name.

```bash
kubectl create namespace metal
llmkube metal token create mac -n metal
```

`token create` uses the current context, `llmkube`, and prints a
`llmkube metal join llmkube://...` line. Run that line on this same Mac:

```bash
llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
```

If this Mac was joined to a cluster before, `join` keeps that join's agent
flags, such as `--host-ip` and `--inference-service-allowlist`, and prints them
as "keeping agent arguments from the previous join". An old allowlist stops
the agent from serving phi-4-mini. Run `llmkube metal leave` first, or drop
them on the join line with `--host-ip= --allowlist=`.

The join string carries the cluster's API server address, `127.0.0.1`, so it
works only on this Mac. `join` writes the agent's own credentials and its
LaunchAgent, starts it, and returns once a pod in the cluster has reached the
agent: it ends with
`Mac mac joined: the cluster reaches it at <address>:9443 (log: ...)`.
If macOS asks whether LLMKube Metal Agent may use the local network, allow
it. If `join` does not finish, see
["If join does not finish"](/docs/guides/macos-metal#if-join-does-not-finish).

```bash
kubectl get mn -n metal
```

## 4. Deploy a model

Phi-4 mini at Q4_K_M (a 2.5 GB file) fits a 16 GB Mac at the default
2048-token context. Save as `phi-4-mini.yaml`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: phi-4-mini
  namespace: metal
spec:
  source: https://huggingface.co/bartowski/microsoft_Phi-4-mini-instruct-GGUF/resolve/main/microsoft_Phi-4-mini-instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: metal
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-4-mini
  namespace: metal
spec:
  modelRef: phi-4-mini
```

```bash
kubectl apply -f phi-4-mini.yaml
kubectl get inferenceservice phi-4-mini -n metal -w
# wait for PHASE Ready; the first start downloads the file to the Mac
```

PHASE stays `Creating` while the agent downloads the 2.5 GB file, which takes
a few minutes on a slow connection; the engine starts seconds after it
arrives.

On the Mac:

```bash
tail -f ~/Library/Logs/llmkube/metal-agent.log
# "starting inference service", "registered endpoint", "started inference service"

# The engine's own output: <model-store>/llama-server-<namespace>-<name>.log
tail -f ~/Library/Application\ Support/llmkube/models/llama-server-metal-phi-4-mini.log
```

## 5. Query it

On the Mac, through the agent's client proxy (`--client-port`, default 9999).
With one model running it reaches that model; with several it picks one per
request, so use the Service to reach a specific model:

```bash
curl -sS http://127.0.0.1:9999/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"What is 2+2?"}],"max_tokens":20}'
```

Through the cluster, port-forward the Service, which selects the relay pod:

```bash
kubectl port-forward -n metal svc/phi-4-mini 8080:8080

# In a second terminal:
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"What is 2+2?"}]}'
```

From a pod, use the Service DNS name:

```bash
kubectl run curl-test --rm -it --restart=Never -n metal --image=curlimages/curl -- \
  curl -sS http://phi-4-mini.metal.svc.cluster.local:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"phi-4-mini","messages":[{"role":"user","content":"What is 2+2?"}]}'
```

The `model` field is the InferenceService's `spec.modelRef`. A name with dots
(`llama-3.1-8b`) becomes a Service name with dashes (`llama-3-1-8b`). The
engine port on the Mac is not reachable from the network by design.

The same endpoints work with an OpenAI client:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:9999/v1", api_key="not-needed")
response = client.chat.completions.create(
    model="phi-4-mini",
    messages=[{"role": "user", "content": "Write a haiku about Kubernetes"}],
)
print(response.choices[0].message.content)
```

## How it works

```
client pod ──▶ phi-4-mini Service ──▶ phi-4-mini-relay pod ──TLS──▶ agent ingress :9443 ──▶ llama-server on 127.0.0.1
               (in the cluster)       (in the cluster)               (on the Mac)

curl on the Mac ──▶ agent client proxy 127.0.0.1:9999 ──▶ llama-server on 127.0.0.1
```

1. The agent checks that the model fits the memory budget, downloads it, and
   starts `llama-server` on `127.0.0.1` with an ephemeral port.
2. It registers its TLS ingress (port 9443, `--ingress-port`) as the Service
   and EndpointSlice `phi-4-mini-agent`.
3. The controller creates the relay Deployment `phi-4-mini-relay` and points
   the `phi-4-mini` Service at it. The relay pins the agent's certificate and
   authenticates with a per-namespace token.

## Clean up

In this order: the engines must be stopped while the agent and its records
still exist.

```bash
kubectl delete -f phi-4-mini.yaml       # the agent stops the engine
llmkube metal leave                     # stops the agent, removes its plist and credentials
llmkube metal remove mac -n metal       # revokes the Mac in the cluster
brew uninstall --zap --cask llmkube-metal-agent
brew uninstall llmkube
container cluster delete --name llmkube
```

`brew uninstall --zap` on its own leaves running engines behind with their
models in memory. Downloaded models stay in
`~/Library/Application Support/llmkube/models`; delete that folder to get the
space back.

## Good to know

- Apple marks `container cluster` as experimental, so its commands and
  defaults can change between releases. This page was tested with
  `container` 1.7.1.
- If the cluster's node stops, for example after `container system stop`,
  Apple's fix is to delete the cluster and create it again
  (`container cluster delete --name llmkube`, then step 1). The new cluster
  has no record of the Mac, so join it again with a new token (step 3): `join`
  stops the agent from the old cluster and starts it for the new one. Models
  already on the Mac are not downloaded again.

## On macOS 13 to 15

Apple's `container` needs macOS 26. On macOS 13 to 15, run the cluster with
minikube and its Docker driver, which needs a running Docker runtime (Docker
Desktop, or another such as Colima). Three steps change:

- What you need: `brew install minikube kubectl helm`, in place of the
  `container` lines.
- Step 1: `minikube start --driver=docker` in place of the two cluster lines,
  and `helm install` without `--set modelCache.mode=perService`: minikube has a
  default StorageClass.
- Clean up: `minikube delete` in place of `container cluster delete`.

The rest is the same.

## More

- Memory budgets, `InsufficientMemory` and `MemoryCheckFailed`, model sources
  on your LAN, and other agent flags: the
  [macOS Metal Agent guide](/docs/guides/macos-metal) and
  [`deployment/macos/README.md`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#configuration).
- Agent health and metrics, on the Mac only: `curl http://127.0.0.1:9090/healthz`
  and `curl -s http://127.0.0.1:9090/metrics | grep llmkube_metal_agent`.
- Issues: [github.com/defilantech/LLMKube/issues](https://github.com/defilantech/LLMKube/issues)
- Discussions: [github.com/defilantech/LLMKube/discussions](https://github.com/defilantech/LLMKube/discussions)

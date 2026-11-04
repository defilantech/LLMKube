# Metal Quickstart: a cluster and the agent on one Mac

This quickstart runs a local Kubernetes cluster (minikube with the Docker
driver) and the LLMKube Metal agent on the same Mac, joins the Mac to that
cluster, and serves a small model with Metal. The agent runs `llama-server`
natively; engines listen on `127.0.0.1` only, the cluster reaches them through
the agent's authenticated TLS ingress and an in-cluster relay, and clients on
the Mac use the agent's client proxy on `127.0.0.1:9999`.

For a cluster on another machine, the network requirements, Local Network
access, running a Mac as a dedicated host, upgrades and troubleshooting, follow
the [macOS Metal Agent guide](https://llmkube.com/docs/guides/macos-metal). The
complete agent reference is [`deployment/macos/README.md`](../../deployment/macos/README.md),
which the release archive also carries under `deployment/macos/`.

## What you need

- An Apple Silicon Mac with 16 GB of memory or more, on macOS 13 or later.
- To be logged in at the Mac's screen: the agent is a LaunchAgent and runs only
  in your login session.
- Ports 9443 and 9090 free on the Mac (the agent's ingress, and its health
  and metrics on `127.0.0.1`).
- Homebrew, and a running Docker runtime for minikube's Docker driver (Docker
  Desktop, or another such as Colima).

```bash
brew install minikube kubectl helm
```

## 1. Start the cluster and install the operator

```bash
minikube start --driver=docker

helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm install llmkube llmkube/llmkube -n llmkube-system --create-namespace
kubectl wait --for=condition=ready pod -l control-plane=controller-manager \
  -n llmkube-system --timeout=120s
```

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

`token create` uses the context `minikube start` set and prints a
`llmkube metal join llmkube://...` line. Run that line on this same Mac:

```bash
llmkube metal join llmkube://eyJ2IjoxLCJzZXJ2ZXIiOi...
```

The join string carries minikube's API server address, `127.0.0.1`, so it
works only on this Mac. `join` writes the agent's own credentials and its
LaunchAgent, starts it, and returns once a pod in minikube has reached the
agent: it ends with
`Mac mac joined: the cluster reaches it at <address>:9443 (log: ...)`.
If macOS asks whether LLMKube Metal Agent may use the local network, allow
it. If `join` does not finish, see
["If join does not finish"](https://llmkube.com/docs/guides/macos-metal#if-join-does-not-finish).

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
               (in minikube)          (in minikube)                  (on the Mac)

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
minikube delete
```

`brew uninstall --zap` on its own leaves running engines behind with their
models in memory. Downloaded models stay in
`~/Library/Application Support/llmkube/models`; delete that folder to get the
space back.

## More

- Memory budgets, `InsufficientMemory` and `MemoryCheckFailed`, model sources
  on your LAN, and other agent flags: the
  [macOS Metal Agent guide](https://llmkube.com/docs/guides/macos-metal) and
  [`deployment/macos/README.md`](../../deployment/macos/README.md#configuration).
- Agent health and metrics, on the Mac only: `curl http://127.0.0.1:9090/healthz`
  and `curl -s http://127.0.0.1:9090/metrics | grep llmkube_metal_agent`.
- Issues: https://github.com/defilantech/llmkube/issues
- Discussions: https://github.com/defilantech/llmkube/discussions

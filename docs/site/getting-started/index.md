---
title: Install LLMKube in 5 minutes
description: Install the LLMKube operator with Helm, deploy a model, and hit an OpenAI-compatible endpoint on kind, Minikube or Docker Desktop, on a GPU cluster, or on an Apple Silicon Mac.
---

# Install in 5 minutes

Install the operator, deploy a model from the catalog, and hit an
OpenAI-compatible endpoint. This works on a fresh kind, Minikube or Docker
Desktop cluster and on real GPU clusters. To serve from an Apple Silicon Mac,
see [Run it on a Mac](#run-it-on-a-mac).

The recording below shows the deploy, wait and query steps on a kind cluster
whose model cache already held the weights. A first deploy also downloads the
model, a few GB for the small catalog models, which takes several minutes on a
slow link.

<AsciinemaCast
  src="/casts/quickstart.cast"
  title="Real-time recording on a kind cluster with the model weights already cached. About 50 seconds wall time, idle waits compressed."
/>

## Prerequisites

- A Kubernetes cluster, v1.27 or later (Minikube, kind, GKE, EKS, AKS or bare metal)
- `kubectl`, pointed at the cluster
- Helm 3
- Cluster admin permissions, to install the CRDs

<DocCallout variant="info" title="Local testing">

Minikube needs at least 4 CPUs and 8 GB of memory for the smaller catalog
models. Start it with `minikube start --cpus 4 --memory 8192`.

</DocCallout>

## 1. Install the CLI

The `llmkube` CLI deploys models with one command and wraps the most-used
kubectl operations.

### Install script (macOS, Linux)

```bash
curl -sSL https://raw.githubusercontent.com/defilantech/LLMKube/main/install.sh | bash
```

It detects the OS and architecture and installs the latest release.

### macOS via Homebrew

```bash
brew install defilantech/tap/llmkube
```

### Windows

Download the Windows binary from the
[latest release](https://github.com/defilantech/LLMKube/releases/latest),
extract it, and add it to your `PATH`.

### Verify

```bash
llmkube version
```

## 2. Install the operator

Install the operator into the cluster with Helm:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update

helm install llmkube llmkube/llmkube \
  --namespace llmkube-system \
  --create-namespace

kubectl get pods -n llmkube-system
```

The chart's options are in the
[Helm chart README](https://github.com/defilantech/LLMKube/tree/main/charts/llmkube).

<DocCallout variant="info" title="Wait for ready">

The operator runs in the `llmkube-system` namespace. Wait for its pods to
reach `Running` before you deploy a model.

</DocCallout>

## 3. Deploy a model

Pick a model from the built-in catalog, or point at any GGUF file on Hugging
Face.

### From the catalog

```bash
llmkube catalog list

llmkube catalog info phi-4-mini

llmkube deploy phi-4-mini
```

### From a Hugging Face URL

```bash
llmkube deploy tinyllama \
  --source https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf \
  --cpu 500m \
  --memory 1Gi
```

### Watch the rollout

`llmkube deploy` waits until the model is ready, up to 10 minutes by default,
and prints the download's progress while the pod fetches the model. For a large
model or a slow link, pass a longer `--timeout`, or `--wait=false` to return at
once. A timeout only stops the wait: the deployment keeps going in the cluster.
To check on it from another terminal, use the name you deployed, `phi-4-mini`
or `tinyllama`:

```bash
MODEL=phi-4-mini   # or tinyllama

llmkube status "$MODEL"

kubectl wait --for=condition=available --timeout=10m "inferenceservice/$MODEL"
```

The pod's `model-downloader` init container downloads the model into the
namespace's model cache, then the server loads it and serves an
OpenAI-compatible HTTP endpoint behind a Service of the same name. The Model
reports `Ready` once its source checks out, before the download, so wait on the
InferenceService, not the Model.

## 4. Test the API

Port-forward the Service (`$MODEL` from step 3) and send a chat request:

```bash
kubectl port-forward "svc/$MODEL" 8080:8080
```

In another terminal:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Explain Kubernetes in one sentence"}],
    "max_tokens": 50
  }'
```

## 5. Use with the OpenAI SDK

LLMKube serves the OpenAI Chat Completions API, so any OpenAI SDK works:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed",  # LLMKube does not require API keys
)

response = client.chat.completions.create(
    model="phi-4-mini",  # the name you deployed
    messages=[{"role": "user", "content": "What is Kubernetes?"}],
)

print(response.choices[0].message.content)
```

LangChain, LlamaIndex, the OpenAI SDKs (Python, Node.js, Go) and any other
tool that speaks the OpenAI API work the same way.

## Run it on a Mac

An Apple Silicon Mac can serve models for the cluster with Metal. The Metal
agent runs on the Mac as a native process, not as a pod, and the operator from
step 2 routes the cluster's requests to it. The cluster can be anywhere the
Mac can reach, including one on the same Mac made with Apple's `container`
tool.

```bash
# On the Mac: the CLI, the Metal agent and llama.cpp
brew install defilantech/tap/llmkube defilantech/tap/llmkube-metal-agent

# Where you have cluster access: a namespace for the Mac, and a join string
kubectl create namespace metal
llmkube metal token create mac-studio -n metal

# On the Mac: paste the line token create printed
llmkube metal join llmkube://...

# Anywhere with cluster access: serve a model from the Mac
llmkube deploy phi-4-mini --accelerator metal -n metal
```

`mac-studio` is the name the cluster gives the Mac. The agent serves the
namespace it joined, so deploy into that namespace. Instead of the CLI, a Model
with `hardware.accelerator: metal` in that namespace does the same thing.

- The [macOS Metal Agent guide](/docs/guides/macos-metal) covers the network
  requirements, memory budgets, more than one Mac, upgrades and
  troubleshooting.
- The [one-Mac quickstart](/docs/getting-started/metal-quickstart)
  runs the whole thing on a single Mac: a cluster made with Apple's
  `container` tool (macOS 26 and later, with minikube for macOS 13 to 15),
  the agent and a first model.

## GPU acceleration

On a cluster with NVIDIA GPUs, deploy with `--gpu`: the operator schedules the
model onto a GPU node and uses a CUDA build of the runtime. The
[GPU setup guide](/docs/guides/gpu-setup)
covers the prerequisites.

```bash
llmkube deploy llama-3.1-8b --gpu --gpu-count 1

llmkube catalog info llama-3.1-8b   # see GPU requirements
```

Benchmark numbers are in the
[README](https://github.com/defilantech/LLMKube#performance).

## Troubleshooting

### Model stuck in "Downloading"

`llmkube deploy` prints the download's progress while it waits. From another
terminal, or after the wait timed out, read the init container's logs:

```bash
kubectl logs <pod-name> -c model-downloader
```

Confirm the cluster has internet access, or that the source URL is reachable
from it. Multi-gigabyte models can take several minutes.

### Pod crashes with OOMKilled

Raise the memory request:

```bash
llmkube deploy <model> --memory 4Gi
```

A safe rule of thumb is at least 1.2 times the GGUF file size, plus room for
the KV cache.

### GPU not detected

Check that the GPU nodes advertise GPUs to the scheduler. With no
`nvidia.com/gpu` count, the NVIDIA device plugin (or the GPU Operator that
installs it) is not running on that node:

```bash
kubectl get nodes -o custom-columns='NAME:.metadata.name,GPUS:.status.allocatable.nvidia\.com/gpu'
```

Check the GPU node label (GKE example):

```bash
kubectl get nodes -l cloud.google.com/gke-accelerator
```

### API requests time out

Confirm the pod is running and read the server's logs:

```bash
kubectl get pods -l app=<model-name>
kubectl logs <pod-name> -c llama-server
```

For larger models or longer prompts, raise the resource requests and the
probe timeouts on the InferenceService.

More cases are in the
[Minikube quickstart's troubleshooting section](/docs/quickstart-minikube#troubleshooting).

## Next

- [Architecture](/docs/concepts/architecture): how the controller and the
  Metal agent split the work
- [CRD reference](/docs/concepts/crds): the fields on Model and
  InferenceService
- [Memory-pressure protection](/docs/memory-pressure-protection): the Metal
  agent's watchdog and eviction model
- [GPU setup](/docs/guides/gpu-setup):
  installing the NVIDIA stack on a fresh cluster

<div align="center">
  <img src="docs/images/logo.png" alt="LLMKube" width="800">

  # LLMKube

  ### Deploy GPU-accelerated LLMs on Kubernetes in 5 minutes

  **17x faster inference** • **Production-ready** • **OpenAI-compatible API**

  <p>
    <a href="https://github.com/defilantech/LLMKube/actions/workflows/helm-chart.yml">
      <img src="https://github.com/defilantech/LLMKube/actions/workflows/helm-chart.yml/badge.svg" alt="Helm Chart CI">
    </a>
    <a href="https://github.com/defilantech/LLMKube/releases">
      <img src="https://img.shields.io/github/v/release/defilantech/LLMKube?label=version" alt="Version">
    </a>
    <a href="LICENSE">
      <img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License">
    </a>
  </p>

  <p>
    <a href="#quick-start">Quick Start</a> •
    <a href="#features">Features</a> •
    <a href="#performance">Performance</a> •
    <a href="ROADMAP.md">Roadmap</a> •
    <a href="#community">Community</a>
  </p>

</div>

---

## Getting Started Video

[![Getting Started with LLMKube](https://img.youtube.com/vi/dmKnkxvC1U8/maxresdefault.jpg)](https://youtu.be/dmKnkxvC1U8)

*Watch: Deploy your first LLM on Kubernetes in 5 minutes*

---

## Why LLMKube?

Running LLMs in production shouldn't require a PhD in distributed systems. LLMKube makes it as easy as deploying any other Kubernetes workload:

- 🚀 **Deploy in minutes** - One command to production-ready GPU inference
- ⚡ **17x faster** - Automatic GPU acceleration with NVIDIA support
- 🔌 **OpenAI-compatible** - Drop-in replacement for OpenAI API
- 📊 **Full observability** - Prometheus + Grafana GPU monitoring included
- 💰 **Cost-optimized** - Auto-scaling and spot instance support
- 🔒 **Air-gap ready** - Perfect for regulated industries and edge deployments

**Perfect for:** AI-powered apps, internal tools, edge computing, air-gapped environments

---

## Quick Start

### 🏃 5-Minute Local Demo (No Cloud Required)

Try LLMKube on your laptop with Minikube - choose your preferred method:

#### Option 1: Using the CLI (Recommended)

Simpler and faster! Just 3 commands:

```bash
# 1. Install the CLI (choose one)
brew install defilantech/tap/llmkube  # macOS (recommended)
# OR: curl -sSL https://raw.githubusercontent.com/defilantech/LLMKube/main/install.sh | bash  # Linux/macOS

# 2. Start Minikube
minikube start --cpus 4 --memory 8192

# 3. Install LLMKube operator with Helm (recommended)
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube \
  --namespace llmkube-system --create-namespace

# 4. Deploy a model from the catalog (one command!)
llmkube deploy phi-3-mini --cpu 500m --memory 1Gi

# Wait for it to be ready (~30 seconds)
kubectl wait --for=condition=available --timeout=300s inferenceservice/phi-3-mini

# Test it!
kubectl port-forward svc/phi-3-mini 8080:8080 &
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"What is Kubernetes?"}],"max_tokens":100}'
```

**New! 📚 Browse the Model Catalog:**
```bash
# See all available pre-configured models
llmkube catalog list

# Get details about a specific model
llmkube catalog info llama-3.1-8b

# Deploy with one command (no need to find GGUF URLs!)
llmkube deploy llama-3.1-8b --gpu
```

<details>
<summary><b>Option 2: Using kubectl (No CLI or Helm)</b></summary>

If you prefer not to install the CLI or Helm, use kubectl with kustomize:

```bash
# Start Minikube
minikube start --cpus 4 --memory 8192

# Install LLMKube operator (note: requires cloning the repo for correct image tags)
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
kubectl apply -k config/default

# Or install just the CRDs and use local controller (see minikube-quickstart.md)

# Deploy a model (copy-paste this whole block)
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
spec:
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf
  format: gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama
spec:
  modelRef: tinyllama
  replicas: 1
  resources:
    cpu: "500m"
    memory: "1Gi"
EOF

# Wait for deployment (~30 seconds for model download)
kubectl wait --for=condition=available --timeout=300s inferenceservice/tinyllama

# Test it!
kubectl run test --rm -i --image=docker.io/curlimages/curl -- \
  curl -X POST http://tinyllama.default.svc.cluster.local:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"What is Kubernetes?"}],"max_tokens":100}'
```
</details>

**See full local setup guide:** [Minikube Quickstart →](docs/minikube-quickstart.md)

### ⚡ Production GPU Deployment (GKE)

Get 17x faster inference with GPU acceleration:

```bash
# 1. Install the CLI
brew tap defilantech/tap && brew install llmkube

# 2. Deploy GKE cluster with GPUs (one command)
cd terraform/gke
terraform init && terraform apply -var="project_id=YOUR_PROJECT"

# 3. Install LLMKube with Helm
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube \
  --namespace llmkube-system \
  --create-namespace

# 4. Deploy a GPU model (single command!)
llmkube deploy llama-3b \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --gpu \
  --gpu-count 1

# 5. Test inference (watch the speed!)
kubectl port-forward svc/llama-3b-service 8080:8080 &
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Explain quantum computing"}]}'
```

---

## Performance

Real benchmarks on GKE with NVIDIA L4 GPU:

| Metric | CPU (Baseline) | GPU (NVIDIA L4) | **Speedup** |
|--------|----------------|-----------------|-------------|
| **Token Generation** | 4.6 tok/s | **64 tok/s** | **17x faster** |
| **Prompt Processing** | 29 tok/s | **1,026 tok/s** | **66x faster** |
| **Total Response Time** | 10.3s | **0.6s** | **17x faster** |
| **Model** | Llama 3.2 3B Q8 | Llama 3.2 3B Q8 | Same quality |

**Cost:** ~$0.35/hour with T4 spot instances (auto-scales to $0 when idle)

### Desktop GPU Benchmarks (Dual RTX 5060 Ti)

Multi-model benchmark on ShadowStack (2x RTX 5060 Ti, 10 iterations, 256 max tokens):

| Model | Size | Gen tok/s | P50 Latency | P99 Latency |
|-------|------|-----------|-------------|-------------|
| Llama 3.2 3B | 3B | **53.3** | 1930ms | 2260ms |
| Mistral 7B v0.3 | 7B | **52.9** | 1912ms | 2071ms |
| Llama 3.1 8B | 8B | **52.5** | 1878ms | 2178ms |

Consistent ~53 tok/s across 3-8B models demonstrates efficient GPU utilization with LLMKube's automatic layer sharding.

📊 [See detailed benchmarks →](docs/gpu-performance-phase0.md)

---

## Features

### ✅ Production-Ready Now

**Core Features:**
- **Kubernetes-native CRDs** - `Model` and `InferenceService` resources
- **Automatic model download** - From HuggingFace, HTTP, or S3
- **Persistent model cache** - Download once, deploy instantly ([guide](docs/MODEL-CACHE.md))
- **OpenAI-compatible API** - `/v1/chat/completions` endpoint
- **Multi-replica scaling** - Horizontal pod autoscaling support
- **Full CLI** - `llmkube deploy/list/status/delete/catalog/cache/queue` commands
- **Model Catalog** - 10 pre-configured popular models (Llama 3.1, Mistral, Qwen, DeepSeek, etc.)
- **GPU Queue Management** - Priority classes, queue position tracking, contention visibility

**GPU Acceleration:**
- ✅ NVIDIA GPU support (T4, L4, A100, RTX)
- ✅ **Multi-GPU support** - Run 13B-70B+ models across 2-8 GPUs ([guide](docs/MULTI-GPU-DEPLOYMENT.md))
- ✅ Automatic layer offloading and tensor splitting
- ✅ Multi-cloud Terraform (GKE, AKS, EKS)
- ✅ Cost optimization (spot instances, auto-scale to 0)

**Observability:**
- ✅ Prometheus + Grafana included
- ✅ GPU metrics (utilization, temp, power, memory)
- ✅ Pre-built dashboards
- ✅ SLO alerts (GPU health, service availability)

### 🔜 Coming Soon

- **Auto-scaling** - Based on queue depth and latency
- **Edge deployment** - K3s, ARM64, air-gapped mode
- **Expanded catalog** - 50+ pre-configured models with benchmarks

See [ROADMAP.md](ROADMAP.md) for the full development plan.

---

## Installation

### Option 1: Helm Chart (Recommended)

```bash
# Add the Helm repository
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update

# Install the chart
helm install llmkube llmkube/llmkube \
  --namespace llmkube-system \
  --create-namespace
```

[See Helm Chart documentation →](charts/llmkube/README.md)

### Option 2: Kustomize

```bash
# Clone the repo to get the correct image configuration
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
kubectl apply -k config/default

# Or use make deploy (requires kustomize installed)
make deploy
```

### Option 3: Local Development

```bash
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
make install  # Install CRDs
make run      # Run controller locally
```

[See Minikube Quickstart →](docs/minikube-quickstart.md)

---

## CLI Installation

The `llmkube` CLI makes deployment simple:

### Quick Install (Recommended)

```bash
# macOS (Homebrew)
brew install defilantech/tap/llmkube

# Linux/macOS (install script)
curl -sSL https://raw.githubusercontent.com/defilantech/LLMKube/main/install.sh | bash
```

<details>
<summary><b>Manual Installation</b></summary>

Download the latest release for your platform from the [releases page](https://github.com/defilantech/LLMKube/releases/latest).

**macOS:**
```bash
# Download and extract (replace VERSION and ARCH as needed)
tar xzf llmkube_*_darwin_*.tar.gz
sudo mv llmkube /usr/local/bin/
```

**Linux:**
```bash
# Download and extract (replace VERSION and ARCH as needed)
tar xzf llmkube_*_linux_*.tar.gz
sudo mv llmkube /usr/local/bin/
```

**Windows:**
Download the `.zip` file, extract, and add to PATH.
</details>

---

## Usage Examples

### Deploy Popular Models

```bash
# TinyLlama (CPU, fast testing)
llmkube deploy tinyllama \
  --source https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf

# Llama 3.2 3B (GPU, production)
llmkube deploy llama-3b \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --gpu --gpu-count 1

# Phi-3 Mini (CPU/GPU)
llmkube deploy phi-3 \
  --source https://huggingface.co/microsoft/Phi-3-mini-4k-instruct-gguf/resolve/main/Phi-3-mini-4k-instruct-q4.gguf
```

### Manage Deployments

```bash
# List all services
llmkube list services

# Check status
llmkube status llama-3b-service

# View GPU queue (services waiting for GPU resources)
llmkube queue -A

# Delete deployment
llmkube delete llama-3b
```

### Use the API

All deployments expose an OpenAI-compatible API:

```python
from openai import OpenAI

# Point to your LLMKube service
client = OpenAI(
    base_url="http://llama-3b-service.default.svc.cluster.local:8080/v1",
    api_key="not-needed"  # LLMKube doesn't require API keys
)

# Use exactly like OpenAI API
response = client.chat.completions.create(
    model="llama-3b",
    messages=[
        {"role": "user", "content": "Explain Kubernetes in one sentence"}
    ]
)

print(response.choices[0].message.content)
```

**Works with:** LangChain, LlamaIndex, OpenAI SDKs (Python, Node, Go)

---

## GPU Setup

### Deploy GKE Cluster with GPU

LLMKube includes production-ready Terraform configs:

```bash
cd terraform/gke

# Deploy cluster with T4 GPUs (recommended for cost)
terraform init
terraform apply -var="project_id=YOUR_GCP_PROJECT"

# Or use L4 GPUs (better performance)
terraform apply \
  -var="project_id=YOUR_GCP_PROJECT" \
  -var="gpu_type=nvidia-l4" \
  -var="machine_type=g2-standard-4"

# Verify GPU nodes
kubectl get nodes -l cloud.google.com/gke-accelerator
```

**Features:**
- ✅ Auto-scales from 0-2 GPU nodes (save money when idle)
- ✅ Spot instances enabled (~70% cheaper)
- ✅ NVIDIA GPU Operator installed automatically
- ✅ Cost alerts configured

**Estimated costs:**
- T4 spot: ~$0.35/hour (~$50-150/month with auto-scaling)
- L4 spot: ~$0.70/hour (~$100-250/month with auto-scaling)

💡 **Important:** Run `terraform destroy` when not in use to avoid charges!

---

## Observability

LLMKube includes full observability out of the box:

```bash
# Access Grafana
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Import GPU dashboard
# Open http://localhost:3000 (admin/prom-operator)
# Import config/grafana/llmkube-gpu-dashboard.json
```

**Metrics included:**
- GPU utilization, temperature, power, memory
- Inference latency and throughput
- Model load times
- Error rates

**Alerts configured:**
- High GPU temperature (>85°C)
- High GPU utilization (>90%)
- Service down
- Controller unhealthy

---

## Architecture

```
┌──────────────┐
│ User / CLI   │
│ llmkube deploy
└──────┬───────┘
       │
       ▼
┌─────────────────────────────────┐
│ Control Plane                   │
│ ┌─────────┐  ┌───────────────┐ │
│ │ Model   │  │ Inference     │ │
│ │ Controller  │ Service      │ │
│ └─────────┘  └───────────────┘ │
└─────────┬───────────────────────┘
          │
          ▼
┌─────────────────────────────────┐
│ Data Plane (GPU Nodes)          │
│ ┌─────────────────────────────┐ │
│ │ Init: Download Model        │ │
│ └─────────────────────────────┘ │
│ ┌─────────────────────────────┐ │
│ │ llama.cpp Server (CUDA)     │ │
│ │ /v1/chat/completions API    │ │
│ └─────────────────────────────┘ │
└─────────────────────────────────┘
```

**Key components:**
1. **Model Controller** - Downloads and validates models
2. **InferenceService Controller** - Creates deployments and services
3. **llama.cpp Runtime** - Efficient CPU/GPU inference
4. **DCGM Exporter** - GPU metrics for Prometheus

---

## Troubleshooting

<details>
<summary><b>Model won't download</b></summary>

```bash
# Check model status
kubectl describe model <model-name>

# Check init container logs
kubectl logs <pod-name> -c model-downloader
```

Common issues:
- HuggingFace URL requires authentication (use direct links)
- Insufficient disk space (increase storage)
- Network timeout (retry will happen automatically)
</details>

<details>
<summary><b>Pod crashes with OOM</b></summary>

```bash
# Check resource limits
kubectl describe pod <pod-name>

# Increase memory in deployment
llmkube deploy <model> --memory 8Gi  # Increase as needed
```

Rule of thumb: Model memory = file size × 1.2
</details>

<details>
<summary><b>GPU not detected</b></summary>

```bash
# Verify GPU operator is running
kubectl get pods -n gpu-operator-resources

# Check device plugin
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# Test GPU with a pod
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  containers:
  - name: cuda
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
  restartPolicy: Never
EOF

kubectl logs gpu-test  # Should show GPU info
```
</details>

---

## FAQ

**Q: Can I run this on my laptop?**
A: Yes! See the [Minikube Quickstart Guide](docs/minikube-quickstart.md). Works great with CPU inference for smaller models.

**Q: What model formats are supported?**
A: Currently GGUF (quantized models from HuggingFace). SafeTensors support coming soon.

**Q: Does this work with private models?**
A: Yes - configure image pull secrets or use PersistentVolumes with `file://` URLs.

**Q: How do I reduce costs?**
A: Use spot instances (default), auto-scale to 0 (default), and run `terraform destroy` when not in use.

**Q: Is this production-ready?**
A: Yes! Single-GPU and multi-GPU deployments are fully supported with monitoring. Advanced auto-scaling coming soon.

**Q: Can I use this in air-gapped environments?**
A: Yes! Pre-download models to PersistentVolumes and use local image registries. Full air-gap support planned for Q1 2026.

---

## Community

We're just getting started! Here's how to get involved:

- 🐛 **Bug reports & features:** [GitHub Issues](https://github.com/defilantech/LLMKube/issues)
- 💬 **Questions & help:** [GitHub Discussions](https://github.com/defilantech/LLMKube/discussions) (coming soon)
- 📖 **Roadmap:** [ROADMAP.md](ROADMAP.md)
- 🤝 **Contributing:** We welcome PRs! See [ROADMAP.md](ROADMAP.md) for priorities

**Help wanted:**
- Additional model formats (SafeTensors)
- AMD/Intel GPU support
- Documentation improvements
- Example applications

---

## Acknowledgments

Built with excellent open-source projects:
- [Kubebuilder](https://kubebuilder.io) - Kubernetes operator framework
- [llama.cpp](https://github.com/ggerganov/llama.cpp) - Efficient LLM inference engine
- [Prometheus](https://prometheus.io) - Metrics and monitoring
- [Helm](https://helm.sh) - Package management

---

## License

Apache 2.0 - See [LICENSE](LICENSE) for details.

---

<div align="center">

**Ready to deploy?** [Try the 5-minute quickstart →](docs/minikube-quickstart.md)

**Have questions?** [Open an issue](https://github.com/defilantech/LLMKube/issues/new)

**⭐ Star us on GitHub** if you find this useful!

</div>

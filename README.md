<div align="center">
  <img src="docs/images/logo.png" alt="LLMKube - Intelligence as Infrastructure" width="800">

  <h1>LLMKube: Kubernetes for Local LLMs</h1>

  <p><em>Intelligence as Infrastructure</em></p>

  <p>
    <strong>Status:</strong> ✅ Phase 1 Complete (GPU + Observability) |
    <strong>Version:</strong> 0.2.0 |
    <strong>License:</strong> Apache 2.0
  </p>
</div>

---

LLMKube is a Kubernetes operator and CLI that makes it easy to deploy, manage, and scale GPU-accelerated LLM inference services. Built for air-gapped environments, edge computing, and production workloads with first-class GPU support.

🎯 **Current Status**: ✅ **Phase 1 Complete** - GPU-accelerated inference with full observability stack deployed. Achieving **17x speedup** (64 tok/s vs 4.6 tok/s CPU) with NVIDIA L4 GPUs. Prometheus metrics, Grafana dashboards, and SLO alerts now available. See [docs/gpu-performance-phase0.md](docs/gpu-performance-phase0.md) for benchmarks and [ROADMAP.md](ROADMAP.md) for next steps.

## Features

### ✅ Available Now
- **Kubernetes-native**: Deploy LLMs using Custom Resource Definitions (CRDs)
- **Automatic Model Download**: Fetch GGUF models from HuggingFace or any HTTP source
- **OpenAI-compatible API**: `/v1/chat/completions` endpoint out of the box
- **CLI Tool**: `llmkube deploy/list/status/delete` commands with full GPU support
- **CPU Inference**: Production-ready with llama.cpp backend
- **Multi-replica Support**: Scale inference services horizontally

### ✅ GPU Foundation (Phase 0 - Complete)
- **GPU Acceleration**: NVIDIA L4 on GKE with CUDA support (**64 tok/s on 3B model**)
- **Multi-GPU API**: Future-proof CRDs for multi-GPU sharding
- **GPU-aware Scheduling**: Tolerations, resource requests, device plugin integration
- **Cost Optimization**: Spot instances, auto-scale to zero enabled
- **Performance**: 17x faster than CPU (0.6s vs 10.3s response time)

### ✅ GPU Inference & Observability (Phase 1 - Complete)
- **GPU Layer Offloading**: Automatic GPU layer offloading with llama.cpp CUDA backend
- **CLI GPU Support**: `llmkube deploy --gpu` for easy GPU deployments
- **Prometheus Metrics**: Full kube-prometheus-stack with DCGM GPU metrics
- **Grafana Dashboards**: GPU utilization, temperature, power, memory monitoring
- **SLO Alerts**: Automated alerts for GPU health (temp, memory, power)
- **E2E Testing**: Comprehensive GPU inference validation suite

### 🔜 Coming Soon (Phase 2+)
- **Multi-Platform CLI**: GoReleaser builds for macOS, Linux, Windows (Phase 2)
- **Multi-GPU Support**: Single-node multi-GPU layer offloading (Phase 2-3)
- **Multi-node GPU Sharding**: Layer-aware model distribution across nodes (Phase 6-7)
- **SLO Enforcement**: GPU auto-scaling and failover (Phase 8-9)
- **Hybrid CPU/GPU**: Intelligent fallback for cost optimization

See [ROADMAP.md](ROADMAP.md) for the complete development plan.

## Installation

### CLI Installation

The `llmkube` CLI makes it easy to deploy and manage LLM inference services. Install it with one command:

#### macOS

**Using Homebrew (Recommended):**
```bash
brew tap defilantech/tap
brew install llmkube
```

**Manual Installation:**
```bash
# Intel (x86_64)
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_darwin_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/

# Apple Silicon (ARM64)
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

#### Linux

**x86_64:**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**ARM64:**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_linux_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

#### Windows

Download the appropriate `.zip` file from the [releases page](https://github.com/defilantech/LLMKube/releases/latest):
- `llmkube_0.2.0_windows_amd64.zip` for x86_64

Extract and add `llmkube.exe` to your PATH.

#### Verify Installation

```bash
llmkube version
# Output: llmkube version 0.2.0
```

#### Build from Source

If you prefer to build from source:

```bash
git clone https://github.com/defilan/llmkube.git
cd llmkube
make build-cli
sudo mv bin/llmkube /usr/local/bin/
```

## Quick Start

**New to LLMKube?** Try our [Minikube Quickstart Guide](docs/minikube-quickstart.md) to run LLMKube locally on your laptop in under 10 minutes - no cloud resources needed!

### Prerequisites

#### For CPU Inference (Basic)
- Kubernetes cluster (v1.11.3+) - works with GKE, EKS, AKS, minikube, kind, K3s
- kubectl configured and connected
- Go 1.24+ (for building from source)
- Docker 17.03+ (for building controller image)

#### For GPU Inference (Recommended)
- **GKE cluster with GPU nodes** (use `terraform/gke` to deploy)
- NVIDIA GPU Operator installed
- GPU device plugin running
- Recommended: NVIDIA T4 GPUs (cost-effective) or L4 (better performance)
- See [GPU Setup Guide](#gpu-setup) below

### 1. Install the Operator

The operator manages Model and InferenceService resources in your cluster.

#### For Local Development (Minikube/Kind)

**Recommended for local clusters:** Run the controller on your host machine to avoid resource constraints. See the [Minikube Quickstart Guide](docs/minikube-quickstart.md) for detailed instructions.

```bash
# Clone the repository
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube

# Install CRDs to your cluster
make install

# Run controller locally (requires Go 1.24+)
make run
```

Keep this terminal open and continue in a new terminal.

#### For Production/Cloud (GKE/EKS/AKS)

Deploy the controller to your cluster:

```bash
# Clone the repository to get manifests
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube

# Install CRDs
make install

# Deploy controller with pre-built image
make deploy IMG=ghcr.io/defilantech/llmkube-controller:0.2.1

# Verify controller is running
kubectl get pods -n llmkube-system
```

### 2. Deploy Your First Model

#### Option A: Using the CLI (Recommended)

The `llmkube` CLI (installed in the previous step) makes deployment simple:

```bash
# Deploy a CPU model
llmkube deploy tinyllama \
  --source https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf \
  --cpu 500m \
  --memory 1Gi

# List deployments
llmkube list services

# Check status
llmkube status tinyllama-service
```

**For GPU deployments** (requires GPU cluster setup - see [GPU Setup Guide](#gpu-setup)):

```bash
llmkube deploy llama-3b-gpu \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --gpu \
  --gpu-count 1 \
  --gpu-memory 8Gi \
  --cpu 2 \
  --memory 4Gi
```

#### Option B: Using kubectl (Advanced)

For full control over CRD specifications, you can use kubectl directly:

```bash
# Create a Model resource
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
  namespace: default
spec:
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf
  format: gguf
  quantization: Q4_K_M
  hardware:
    accelerator: cpu
  resources:
    cpu: "2"
    memory: "2Gi"
EOF

# Create an InferenceService
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama
  namespace: default
spec:
  modelRef: tinyllama
  replicas: 1
  resources:
    cpu: "500m"
    memory: "1Gi"
  endpoint:
    port: 8080
    type: ClusterIP
EOF
```

**Note**: The kubectl method allows you to customize all CRD fields. See `config/samples/` for more examples.

### 3. Test the Inference Endpoint

```bash
# Check that the service is ready
kubectl get inferenceservice tinyllama

# Create a test pod to call the API
kubectl run test-curl --image=curlimages/curl --command -- sleep 3600

# Send a chat completion request
kubectl exec test-curl -- curl -X POST \
  http://tinyllama.default.svc.cluster.local:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "tinyllama",
    "messages": [
      {"role": "user", "content": "What is 2+2?"}
    ],
    "max_tokens": 50
  }'

# Clean up test pod
kubectl delete pod test-curl
```

Expected response:
```json
{
  "choices": [{
    "finish_reason": "stop",
    "message": {
      "role": "assistant",
      "content": "\nYes, 2+2 is 4 in the English numeral system."
    }
  }],
  "model": "tinyllama",
  "usage": {
    "completion_tokens": 18,
    "prompt_tokens": 22,
    "total_tokens": 40
  }
}
```

### 4. Access from Outside the Cluster (Optional)

```bash
# Option 1: Port forward
kubectl port-forward svc/tinyllama-service 8080:8080

# Then test locally
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "Hello!"}]}'

# Option 2: Expose as LoadBalancer (GKE/EKS/AKS)
kubectl patch inferenceservice tinyllama-service -p '{"spec":{"endpoint":{"type":"LoadBalancer"}}}'
kubectl get svc tinyllama-service  # Get external IP
```

## Performance

### CPU Baseline
Observed with **TinyLlama 1.1B Q4_K_M** on GKE CPU nodes (n2-standard-2):

- **Model Size**: 637.8 MiB
- **Prompt Processing**: ~29 tokens/sec
- **Token Generation**: ~18.5 tokens/sec
- **Cold Start** (with download): ~5 seconds
- **Warm Start**: <1 second
- **Latency P50**: ~1.5s for simple queries

### GPU Performance ✅ (Phase 1)
Observed with **Llama 3.2 3B Q8_0** on GKE with NVIDIA L4 GPU:

- **Model Size**: 3.2 GiB
- **Prompt Processing**: ~1,026 tokens/sec (**66x faster than CPU**)
- **Token Generation**: ~64 tokens/sec (**17x faster than CPU**)
- **GPU Layers**: 29/29 layers offloaded automatically
- **GPU Memory**: 4.2 GB VRAM used
- **Power Usage**: ~35W
- **Temperature**: 56-58°C
- **Total Response Time**: ~0.6s (**17x faster than CPU's 10.3s**)

### Future Improvements
Performance will improve further with:
- Multi-GPU support (Phase 2-3)
- KV cache optimization
- Multi-node GPU sharding for large models (Phase 6-7)

## Examples

### Deploy Different Models

#### Phi-3 Mini (3.8B parameters)
```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: phi-3-mini
spec:
  source: https://huggingface.co/microsoft/Phi-3-mini-4k-instruct-gguf/resolve/main/Phi-3-mini-4k-instruct-q4.gguf
  format: gguf
  quantization: Q4
  resources:
    cpu: "4"
    memory: "8Gi"
```

#### With GPU Acceleration ✅ (Working - Phase 1)

**Using CLI** (Recommended):
```bash
# Deploy Llama 3.2 3B with GPU (verified working)
llmkube deploy llama-3b-gpu \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --gpu \
  --gpu-count 1 \
  --gpu-memory 8Gi \
  --quantization Q8_0
```

**Using kubectl**:
```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-3b-gpu
spec:
  source: https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf
  format: gguf
  quantization: Q8_0
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
      vendor: nvidia
      layers: -1  # Offload all layers to GPU (auto: 29/29 layers)
  resources:
    cpu: "2"
    memory: "4Gi"
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3b-gpu-service
spec:
  modelRef: llama-3b-gpu
  replicas: 1
  image: ghcr.io/ggerganov/llama.cpp:server-cuda
  resources:
    gpu: 1
    gpuMemory: "8Gi"
    cpu: "2"
    memory: "4Gi"
  endpoint:
    port: 8080
    type: ClusterIP
```

**Performance** (NVIDIA L4):
- **64 tok/s generation** (17x faster than CPU)
- **1,026 tok/s prompt processing**
- **29/29 layers** offloaded automatically
- **4.2GB GPU memory** used

#### Multi-GPU Example (Future)
```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-70b
spec:
  source: https://huggingface.co/.../llama-70b-q4.gguf
  format: gguf
  quantization: Q4
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 4  # Multi-GPU sharding
      vendor: nvidia
      layers: -1  # Auto-detect optimal split
      sharding:
        strategy: layer
        # Auto-split 80 layers across 4 GPUs: [0-19, 20-39, 40-59, 60-79]
  resources:
    cpu: "16"
    memory: "64Gi"
```

### Observability

LLMKube includes comprehensive observability with Prometheus and Grafana for GPU metrics and inference monitoring.

#### Accessing Grafana

```bash
# Get Grafana external IP (if using LoadBalancer)
kubectl get svc -n monitoring kube-prometheus-stack-grafana

# Or port-forward for local access
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Access at http://localhost:3000
# Default credentials: admin / prom-operator
```

#### GPU Metrics Available

LLMKube integrates with NVIDIA DCGM (Data Center GPU Manager) to collect GPU metrics:

- **GPU Utilization**: `DCGM_FI_DEV_GPU_UTIL` - GPU usage percentage
- **GPU Temperature**: `DCGM_FI_DEV_GPU_TEMP` - Temperature in Celsius
- **GPU Memory**: `DCGM_FI_DEV_FB_USED`, `DCGM_FI_DEV_FB_FREE` - Memory usage
- **GPU Power**: `DCGM_FI_DEV_POWER_USAGE` - Power consumption in Watts

#### Importing the GPU Dashboard

```bash
# Dashboard is pre-configured at:
# config/grafana/llmkube-gpu-dashboard.json

# To import:
# 1. Access Grafana (see above)
# 2. Go to Dashboards > Import
# 3. Upload config/grafana/llmkube-gpu-dashboard.json
# 4. Select Prometheus datasource
# 5. Click Import

# Dashboard includes:
# - GPU Utilization gauge
# - GPU Temperature gauge
# - GPU Power Usage gauge
# - GPU Memory timeseries
# - GPU Utilization over time
# - GPU Power over time
```

#### SLO Alerts

Alerts are automatically configured for GPU health monitoring:

```bash
# View configured alerts
kubectl get prometheusrule llmkube-alerts -n monitoring

# Alerts include:
# - GPUHighUtilization: >90% for 5 minutes
# - GPUHighTemperature: >85°C for 2 minutes (critical)
# - GPUMemoryPressure: >90% memory for 5 minutes
# - GPUPowerLimit: >250W for 10 minutes
# - InferenceServiceDown: Service unavailable for 1 minute
# - ControllerDown: Controller down for 2 minutes
```

#### Basic Monitoring

```bash
# Check model status
kubectl get models

# Check inference service status
kubectl get inferenceservices

# View controller logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager

# View inference pod logs
kubectl logs -l app=tinyllama-service
```

## GPU Setup

### Deploying GKE Cluster with GPU Support

LLMKube includes Terraform configs for deploying a production-ready GKE cluster with NVIDIA GPU support:

```bash
cd terraform/gke

# Configure your GCP project
export TF_VAR_project_id="your-gcp-project-id"

# Review the configuration
# - Default: T4 GPUs (cost-effective)
# - Auto-scales from 0-2 GPU nodes (save money when idle)
# - Spot instances enabled by default (~70% cheaper)

# Initialize Terraform
terraform init

# Preview changes
terraform plan

# Deploy cluster (takes ~10-15 minutes)
terraform apply

# Verify GPU nodes
kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-tesla-t4

# Check NVIDIA device plugin is running
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds
```

### GPU Configuration Options

Edit `terraform/gke/variables.tf` or pass via command line:

```bash
# Use L4 GPUs instead of T4 (better performance, higher cost)
terraform apply -var="gpu_type=nvidia-l4" -var="machine_type=g2-standard-4"

# Disable spot instances for production stability
terraform apply -var="use_spot=false"

# Scale GPU nodes (min=0 saves money, max=4 for larger workloads)
terraform apply -var="min_gpu_nodes=0" -var="max_gpu_nodes=4"
```

### Cost Management

**Important**: GPU nodes are expensive. Follow these best practices:

1. **Auto-scale to Zero**: Default config scales to 0 GPU nodes when idle
2. **Use Spot Instances**: ~70% cheaper (default: enabled)
3. **Monitor Costs**: Set GCP billing alerts
4. **Teardown When Done**:
   ```bash
   cd terraform/gke
   terraform destroy  # IMPORTANT: Run this when not in use!
   ```

**Estimated Costs** (us-central1):
- T4 Spot: ~$0.35/hr per GPU (~$250/mo if running 24/7)
- L4 Spot: ~$0.70/hr per GPU (~$500/mo if running 24/7)
- With auto-scale to 0: ~$50-150/mo for dev/test usage

### Verifying GPU Setup

```bash
# Check GPU nodes are ready
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"

# Deploy a test GPU workload
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  nodeSelector:
    cloud.google.com/gke-accelerator: nvidia-tesla-t4
EOF

# Check GPU is detected
kubectl logs gpu-test

# Clean up
kubectl delete pod gpu-test
```

## Development

### Building from Source

```bash
# Build the controller
make docker-build docker-push IMG=<your-registry>/llmkube:tag

# Deploy your build
make deploy IMG=<your-registry>/llmkube:tag

# Run tests
make test

# Run locally (without deploying to cluster)
make run
```

### Project Structure

```
llmkube/
├── api/v1alpha1/          # CRD definitions
│   ├── model_types.go
│   └── inferenceservice_types.go
├── cmd/
│   ├── main.go            # Operator entrypoint
│   └── cli/               # CLI tool
├── config/
│   ├── crd/               # Generated CRD manifests
│   ├── manager/           # Controller deployment
│   └── samples/           # Example resources
├── internal/controller/   # Reconciliation logic
│   ├── model_controller.go
│   └── inferenceservice_controller.go
└── docs/                  # Documentation

```

### Contributing

See [ROADMAP.md](ROADMAP.md) for current priorities and upcoming features.

**Completed (Phase 1)** ✅:
- ~~Prometheus metrics integration~~ - Done with kube-prometheus-stack
- ~~Grafana dashboard templates~~ - Done with GPU metrics dashboard
- ~~E2E test suite~~ - Done with comprehensive GPU validation
- ~~GPU support (NVIDIA)~~ - Done with L4 GPU on GKE

**Help Wanted (Phase 2+)**:
- Multi-platform CLI builds (GoReleaser setup)
- Multi-GPU single-node support
- AMD GPU support (ROCm)
- Intel GPU support (oneAPI)
- Additional Grafana dashboards (inference metrics, cost tracking)

### Uninstalling

```bash
# Delete all inference services and models
kubectl delete inferenceservices --all
kubectl delete models --all

# Uninstall the operator
make undeploy

# Remove CRDs
make uninstall
```

## Architecture

LLMKube follows the Kubernetes operator pattern:

```
┌─────────────────────────────────────────────────────────┐
│  User / CLI                                             │
│  llmkube deploy / kubectl apply                         │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│  Control Plane (llmkube-controller-manager)             │
│  ┌───────────────┐  ┌──────────────────────┐           │
│  │ Model CRD     │  │ InferenceService CRD │           │
│  │ Controller    │  │ Controller           │           │
│  └───────────────┘  └──────────────────────┘           │
└────────────────────┬────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────┐
│  Data Plane (Per Node)                                  │
│  ┌────────────────────────────────────────────────┐    │
│  │ Init Container: model-downloader               │    │
│  │   • Downloads GGUF from source URL             │    │
│  │   • Validates file integrity                   │    │
│  └────────────────────────────────────────────────┘    │
│  ┌────────────────────────────────────────────────┐    │
│  │ Main Container: llama-server                   │    │
│  │   • Loads model from shared volume             │    │
│  │   • Serves /v1/chat/completions API            │    │
│  │   • OpenAI-compatible responses                │    │
│  └────────────────────────────────────────────────┘    │
│  ┌────────────────────────────────────────────────┐    │
│  │ Service: ClusterIP / LoadBalancer              │    │
│  │   • Routes traffic to inference pods           │    │
│  └────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

### Key Components

1. **Model Controller**: Manages model lifecycle, downloads, validation
2. **InferenceService Controller**: Creates deployments, services, manages replicas
3. **llama.cpp Runtime**: Efficient CPU/GPU inference engine
4. **Init Container Pattern**: Separates model download from serving

## Troubleshooting

### Model won't download
```bash
# Check model status
kubectl describe model <model-name>

# Check init container logs
kubectl logs <pod-name> -c model-downloader
```

### Inference pod crashing
```bash
# Check resource limits
kubectl describe pod <pod-name>

# View server logs
kubectl logs <pod-name> -c llama-server

# Common issues:
# - Insufficient memory (increase resources.memory)
# - Model file corrupted (delete and redeploy)
# - Wrong model format (ensure GGUF format)
```

### API not responding
```bash
# Verify service exists
kubectl get svc

# Check endpoint is configured
kubectl get inferenceservice <name> -o yaml

# Test from within cluster
kubectl run test --rm -it --image=curlimages/curl -- \
  curl http://<service-name>:8080/health
```

## FAQ

**Q: Can I run this on my laptop?**
A: Yes! See our [Minikube Quickstart Guide](docs/minikube-quickstart.md) for step-by-step instructions. Works with minikube or kind. Note that CPU inference is slower than GPU for large models, but TinyLlama runs well locally.

**Q: What model formats are supported?**
A: Currently only GGUF. SafeTensors and HF format coming in Q2 2026.

**Q: Does this work with private models?**
A: Yes, but you'll need to configure image pull secrets for private registries or use file:// URLs with PersistentVolumes.

**Q: How do I monitor performance?**
A: Full observability is available! We have:
- Prometheus + Grafana with DCGM GPU metrics (utilization, temp, power, memory)
- Pre-built GPU metrics dashboard in `config/grafana/llmkube-gpu-dashboard.json`
- SLO alerts for GPU health monitoring
- Basic inference metrics in response JSON

**Q: Can I use this in production?**
A: Yes for GPU-accelerated inference! Observability is complete (Phase 1). For production-critical workloads, we recommend waiting for:
- Multi-GPU support and auto-scaling (Phase 2-5)
- Advanced SLO enforcement and failover (Phase 9-10)
Current status: Production-ready for single-GPU deployments with monitoring.

## Resources

- **Roadmap**: [ROADMAP.md](ROADMAP.md)
- **Examples**: [config/samples/](config/samples/)
- **Kubebuilder Docs**: https://book.kubebuilder.io

## Community

- **GitHub Issues**: Bug reports and feature requests
- **Discussions**: Q&A and community help (coming soon)
- **Slack/Discord**: Community chat (coming soon)

## Acknowledgments

Built with:
- [Kubebuilder](https://kubebuilder.io) - Kubernetes operator framework
- [llama.cpp](https://github.com/ggerganov/llama.cpp) - Efficient LLM inference
- [Cobra](https://github.com/spf13/cobra) - CLI framework

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.


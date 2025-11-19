# Minikube Quickstart Guide

Get LLMKube running on your local machine with Minikube in under 10 minutes. This guide is perfect for local development, testing, and learning without needing cloud resources.

## Prerequisites

### System Requirements

- **CPU**: 4+ cores (2 for Minikube, 2+ for LLM inference)
- **RAM**: 8GB+ (4GB for Minikube, 2GB+ for model)
- **Disk**: 20GB+ free space (for Minikube VM + model downloads)
- **OS**: macOS, Linux, or Windows

### Required Tools

#### 1. Install Minikube

**macOS:**
```bash
# Using Homebrew
brew install minikube

# Or download directly
curl -LO https://storage.googleapis.com/minikube/releases/latest/minikube-darwin-amd64
sudo install minikube-darwin-amd64 /usr/local/bin/minikube
```

**Linux:**
```bash
curl -LO https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
sudo install minikube-linux-amd64 /usr/local/bin/minikube
```

**Windows:**
```powershell
# Using Chocolatey
choco install minikube

# Or download from https://minikube.sigs.k8s.io/docs/start/
```

#### 2. Install kubectl

**macOS:**
```bash
brew install kubectl
```

**Linux:**
```bash
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install kubectl /usr/local/bin/kubectl
```

**Windows:**
```powershell
choco install kubernetes-cli
```

#### 3. Verify Installations

```bash
minikube version
# Expected: minikube version: v1.32.0 (or newer)

kubectl version --client
# Expected: Client Version: v1.28.0 (or newer)
```

## Step 1: Start Minikube

Start Minikube with sufficient resources for running a small LLM:

```bash
# Recommended configuration for LLMKube
minikube start \
  --cpus=4 \
  --memory=6144 \
  --disk-size=20g \
  --driver=docker

# Wait for cluster to be ready
minikube status
```

**Driver Options:**
- `docker` (recommended): Runs on Docker Desktop (macOS/Windows/Linux)
- `hyperkit` (macOS): Native macOS hypervisor
- `virtualbox`: Cross-platform, requires VirtualBox
- `kvm2` (Linux): KVM virtualization

**Troubleshooting:**
- If `--driver=docker` fails, try `--driver=virtualbox` or `--driver=hyperkit` (macOS)
- On Linux, you may need to run `minikube start` without sudo
- On Windows, ensure Hyper-V or VirtualBox is installed

**Verify cluster:**
```bash
kubectl cluster-info
kubectl get nodes

# Expected output:
# NAME       STATUS   ROLES           AGE   VERSION
# minikube   Ready    control-plane   1m    v1.28.0
```

## Step 2: Install LLMKube Operator

### Option A: Deploy to Minikube (Recommended)

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
# Expected: llmkube-controller-manager-xxxxx   1/1   Running
```

### Option B: Run Controller Locally (Development)

For development or if you prefer running the controller on your host machine:

```bash
# Clone the repository
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube

# Install CRDs
make install

# Run controller locally (requires Go 1.24+)
make run
```

This runs the controller outside the cluster, which is useful for debugging.

## Step 3: Deploy Your First Model

### Option A: Using the CLI (Recommended)

The `llmkube` CLI makes deployment simple. First, install it:

**macOS:**
```bash
# Using Homebrew
brew tap defilantech/tap
brew install llmkube

# Or download binary directly
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux:**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.2.0_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Verify installation:**
```bash
llmkube version
# Output: llmkube version 0.2.0
```

**Deploy TinyLlama:**
```bash
llmkube deploy tinyllama \
  --source https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf \
  --cpu 500m \
  --memory 1Gi

# Check deployment status
llmkube list services

# Check detailed status
llmkube status tinyllama-service
```

### Option B: Using kubectl (Advanced)

For full control over CRD specifications:

```bash
# Create Model resource
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

# Create InferenceService
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama-service
  namespace: default
spec:
  modelRef: tinyllama
  replicas: 1
  resources:
    cpu: "500m"      # Reduced for Minikube
    memory: "1Gi"    # Suitable for local testing
  endpoint:
    port: 8080
    type: ClusterIP
EOF
```

**What happens:**
1. Model controller downloads the GGUF file (~638MB) from HuggingFace
2. InferenceService controller creates a Deployment and Service
3. Pod starts with init container to load the model
4. llama-server container serves the OpenAI-compatible API

**Monitor deployment:**
```bash
# Watch model download
kubectl get model tinyllama -w
# Wait for STATUS: Ready

# Watch service deployment
kubectl get inferenceservice tinyllama-service -w
# Wait for STATUS: Available

# Check pod status
kubectl get pods -l app=tinyllama-service

# View logs
POD=$(kubectl get pod -l app=tinyllama-service -o jsonpath='{.items[0].metadata.name}')
kubectl logs $POD -c model-downloader -f  # Model download progress
kubectl logs $POD -c llama-server -f       # Server startup
```

## Step 4: Test the Inference Endpoint

### Port Forward to Access API

```bash
# Forward the service port to localhost
kubectl port-forward svc/tinyllama-service 8080:8080
```

Keep this terminal open. In a **new terminal**, test the API:

### Send a Test Request

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "tinyllama",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is Kubernetes in one sentence?"}
    ],
    "max_tokens": 50
  }'
```

**Expected Response:**
```json
{
  "id": "chatcmpl-xxxx",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "tinyllama",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "Kubernetes is an open-source container orchestration platform that automates deployment, scaling, and management of containerized applications."
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 28,
    "total_tokens": 53
  }
}
```

### Performance Expectations

On a typical laptop (4 cores, 8GB RAM):
- **Model Load Time**: ~5-10 seconds (first request)
- **Token Generation**: ~10-20 tokens/sec
- **Response Time**: 2-5 seconds for simple queries
- **Memory Usage**: ~1.5GB total

## Step 5: Explore and Experiment

### Scale the Service

```bash
# Scale to 2 replicas
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"replicas":2}}' --type=merge

# Watch pods scale
kubectl get pods -l app=tinyllama-service -w
```

### Monitor Logs

```bash
# View inference logs
kubectl logs -l app=tinyllama-service --tail=50 -f

# View controller logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager -f
```

### Check Resource Usage

```bash
# View Minikube resource usage
minikube ssh
top
free -h
exit

# Or use kubectl top (requires metrics-server)
minikube addons enable metrics-server
kubectl top nodes
kubectl top pods -l app=tinyllama-service
```

## Troubleshooting

### Pod Stuck in Pending

```bash
# Check pod events
kubectl describe pod -l app=tinyllama-service

# Common issues:
# - "Insufficient memory": Increase Minikube memory
# - "Insufficient cpu": Increase Minikube CPUs
```

**Solution:**
```bash
# Stop Minikube
minikube stop

# Restart with more resources
minikube start --cpus=6 --memory=8192

# Redeploy your service
kubectl delete pod -l app=tinyllama-service
```

### Model Download Fails

```bash
# Check init container logs
kubectl logs $POD -c model-downloader

# Common issues:
# - Network timeout: Check internet connection
# - Disk full: Check Minikube disk space
```

**Check disk space:**
```bash
minikube ssh
df -h
exit
```

**Solution:**
```bash
# Increase disk size (requires recreation)
minikube delete
minikube start --cpus=4 --memory=6144 --disk-size=30g
```

### Pod Crashes with OOMKilled

```bash
kubectl describe pod -l app=tinyllama-service
# Look for "Last State: Terminated (OOMKilled)"
```

**Solution:**
```bash
# Increase service memory
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"resources":{"memory":"2Gi"}}}' --type=merge
```

### API Connection Refused

```bash
# Verify service is running
kubectl get svc tinyllama-service

# Verify pod is ready
kubectl get pods -l app=tinyllama-service

# Check if port-forward is active
# Kill and restart: kubectl port-forward svc/tinyllama-service 8080:8080
```

### Slow Performance

**Expected for CPU-only inference on laptops:**
- 10-20 tok/s is normal for TinyLlama on CPU
- First request is slower (model loading)
- Subsequent requests are faster (warm cache)

**Optimization tips:**
```bash
# Reduce model size
# Use Q4_K_S instead of Q4_K_M (smaller, faster, slightly lower quality)

# Increase CPU allocation (if you have cores to spare)
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"resources":{"cpu":"1000m"}}}' --type=merge
```

## Next Steps

### Deploy a Different Model

Try Phi-3 Mini (requires more memory):

```bash
# Create larger Minikube cluster
minikube delete
minikube start --cpus=6 --memory=10240 --disk-size=30g

# Deploy Phi-3 Mini
kubectl apply -f - <<EOF
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
EOF

# Create service
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-3-mini-service
spec:
  modelRef: phi-3-mini
  replicas: 1
  resources:
    cpu: "1"
    memory: "2Gi"
  endpoint:
    port: 8080
    type: ClusterIP
EOF
```

### Integrate with Applications

The API is OpenAI-compatible. Example with Python:

```python
from openai import OpenAI

# Point to your local service
client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="tinyllama",
    messages=[
        {"role": "user", "content": "Hello, how are you?"}
    ]
)

print(response.choices[0].message.content)
```

### Explore GPU Support

While Minikube supports GPU passthrough, it's complex to set up. For GPU inference:
- Use GKE with GPUs (see [GPU Setup Guide](gpu-setup-guide.md))
- Or use a cloud provider with GPU support
- GPU provides 5-10x speedup over CPU

## Clean Up

### Delete Resources

```bash
# Delete inference service
kubectl delete inferenceservice tinyllama-service

# Delete model
kubectl delete model tinyllama

# Delete operator (optional)
kubectl delete namespace llmkube-system
```

### Stop Minikube

```bash
# Pause Minikube (keeps state, fast restart)
minikube pause

# Stop Minikube (saves state)
minikube stop

# Delete Minikube (removes all data)
minikube delete
```

## Useful Minikube Commands

```bash
# Access Minikube dashboard
minikube dashboard

# SSH into Minikube VM
minikube ssh

# Check Minikube IP
minikube ip

# Access service via Minikube (alternative to port-forward)
minikube service tinyllama-service --url

# View Minikube logs
minikube logs

# Enable addons
minikube addons list
minikube addons enable metrics-server
minikube addons enable ingress
```

## Learn More

- **Main Documentation**: [README.md](../README.md)
- **General Quickstart**: [examples/quickstart/README.md](../examples/quickstart/README.md)
- **GPU Setup** (for cloud): [docs/gpu-setup-guide.md](gpu-setup-guide.md)
- **Roadmap**: [ROADMAP.md](../ROADMAP.md)
- **Contributing**: [CONTRIBUTING.md](../CONTRIBUTING.md)

## Support

- **GitHub Issues**: [LLMKube Issues](https://github.com/defilantech/LLMKube/issues)
- **Minikube Docs**: https://minikube.sigs.k8s.io/docs/
- **Kubernetes Docs**: https://kubernetes.io/docs/

---

**Congratulations!** You're now running LLMs locally with LLMKube on Minikube.

**Time to first inference**: ~10 minutes
**Cost**: $0 (runs entirely on your laptop)

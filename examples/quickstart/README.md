# Quickstart: Deploy TinyLlama in 5 Minutes

This guide walks you through deploying your first LLM inference service with LLMKube.

## Prerequisites

- Kubernetes cluster (v1.11.3+)
  - **minikube**: `minikube start --cpus=4 --memory=6144` (see [Minikube Quickstart](../../docs/minikube-quickstart.md))
  - **kind**: `kind create cluster`
  - **GKE/EKS/AKS**: Any managed Kubernetes
- `kubectl` configured and connected
- At least 2GB free memory on your nodes
- Go 1.24+ (if running controller locally)

## What You'll Deploy

- **Model**: TinyLlama 1.1B (Q4_K_M quantized, ~638MB)
- **API**: OpenAI-compatible chat completions endpoint
- **Performance**: ~18 tokens/sec on CPU, <1s warm start

## Step 1: Install LLMKube Operator

### For Minikube/Kind (Local Development)

**Recommended:** Run the controller locally to avoid resource constraints:

```bash
# Clone the repository
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube

# Install CRDs
make install

# Run controller locally (requires Go 1.24+)
make run
```

Keep this terminal open and continue in a new terminal. See the [Minikube Quickstart](../../docs/minikube-quickstart.md) for details.

### For Cloud Kubernetes (GKE/EKS/AKS)

Deploy the controller to your cluster:

```bash
# Option 1: Using Helm (Recommended)
helm install llmkube https://github.com/defilantech/LLMKube/releases/download/v0.3.3/llmkube-0.3.3.tgz \
  --namespace llmkube-system --create-namespace

# Option 2: Using Kustomize
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube
kubectl apply -k config/default

# Wait for operator to be ready
kubectl wait --for=condition=available --timeout=60s \
  deployment/llmkube-controller-manager -n llmkube-system
```

**Verify installation:**
```bash
kubectl get pods -n llmkube-system
# Should show: llmkube-controller-manager-xxxx   1/1   Running
```

## Step 2: Deploy TinyLlama Model

### Option A: Using the CLI (Recommended)

The `llmkube` CLI makes deployment simple:

**Install the CLI:**

**macOS:**
```bash
# Using Homebrew
brew tap defilantech/tap
brew install llmkube

# Or download binary directly
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.3.3_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux:**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/latest/download/llmkube_0.3.3_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Verify installation:**
```bash
llmkube version
# Output: llmkube version 0.3.3
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

**What happens:**
- LLMKube downloads the GGUF file (~638MB) from HuggingFace
- Creates a Model and InferenceService resource automatically
- Deploys the inference pod with appropriate resources
- Sets up an OpenAI-compatible API endpoint

### Option B: Using kubectl (Advanced)

For full control over CRD specifications, create `tinyllama-model.yaml`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
  namespace: default
spec:
  # HuggingFace download URL
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf

  format: gguf
  quantization: Q4_K_M

  # CPU-only inference
  hardware:
    accelerator: cpu

  # Resource allocation for model processing
  resources:
    cpu: "2"
    memory: "2Gi"
```

Create `tinyllama-service.yaml`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama-service
  namespace: default
spec:
  # Reference the model we created
  modelRef: tinyllama

  # Single replica (scale up later)
  replicas: 1

  # Container resources
  resources:
    cpu: "500m"
    memory: "1Gi"

  # OpenAI-compatible endpoint
  endpoint:
    port: 8080
    type: ClusterIP
```

**Apply:**
```bash
kubectl apply -f tinyllama-model.yaml
kubectl apply -f tinyllama-service.yaml

# Watch model download progress
kubectl get model tinyllama -w
# Wait until STATUS shows "Ready"

# Wait for pod to be ready
kubectl wait --for=condition=ready --timeout=120s \
  pod -l app=tinyllama-service

# Verify service is running
kubectl get inferenceservice tinyllama-service
# STATUS should show "Available"
```

## Step 3: Monitor Deployment

```bash
# Check model status
kubectl get models

# Check service status
kubectl get inferenceservices

# View pod logs
kubectl logs -l app=tinyllama-service --tail=50 -f
```

## Step 4: Test the API

### Option A: From Inside the Cluster

```bash
# Create a test pod
kubectl run test-curl --image=curlimages/curl --rm -it -- sh

# Inside the pod, run:
curl -X POST http://tinyllama-service.default.svc.cluster.local:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "tinyllama",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "What is 2+2?"}
    ],
    "max_tokens": 50
  }'

# Exit the pod
exit
```

### Option B: Port Forward to Localhost

```bash
# Forward port to your local machine
kubectl port-forward svc/tinyllama-service 8080:8080
```

**In another terminal:**
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "user", "content": "Explain Kubernetes in one sentence."}
    ],
    "max_tokens": 100
  }'
```

**Expected Response:**
```json
{
  "id": "chatcmpl-xxxx",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "tinyllama",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Kubernetes is an open-source container orchestration platform that automates deployment, scaling, and management of containerized applications."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 28,
    "total_tokens": 40
  }
}
```

## Step 5: Scale and Monitor

### Scale replicas:
```bash
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"replicas":3}}' --type=merge

# Watch pods scale up
kubectl get pods -l app=tinyllama-service -w
```

### Monitor logs:
```bash
# View inference pod logs
kubectl logs -l app=tinyllama-service --tail=50 -f

# View controller logs
kubectl logs -n llmkube-system \
  deployment/llmkube-controller-manager -f
```

### Check status:
```bash
# Model status
kubectl describe model tinyllama

# Service status
kubectl describe inferenceservice tinyllama-service
```

## Troubleshooting

### Model stays in "Downloading" state
```bash
# Check model controller logs
kubectl describe model tinyllama

# Common issues:
# - Network connectivity (check firewall/proxy)
# - Insufficient disk space
# - HuggingFace URL changed
```

### Pod crashes with OOMKilled
```bash
kubectl describe pod -l app=tinyllama-service

# Solution: Increase memory
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"resources":{"memory":"2Gi"}}}' --type=merge
```

### API returns 404
```bash
# Verify service exists
kubectl get svc tinyllama-service

# Verify endpoint is correct
kubectl get inferenceservice tinyllama-service -o yaml | grep -A5 endpoint

# Test pod readiness
kubectl get pods -l app=tinyllama-service
```

## Next Steps

### Deploy a Larger Model

Try Phi-3 Mini (3.8B parameters):

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

### Expose Externally

For cloud deployments (GKE, EKS, AKS):

```bash
kubectl patch inferenceservice tinyllama-service \
  -p '{"spec":{"endpoint":{"type":"LoadBalancer"}}}' --type=merge

# Get external IP
kubectl get svc tinyllama-service
```

### Integrate with Applications

The API is OpenAI-compatible, so you can use existing SDKs:

**Python:**
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://tinyllama-service.default.svc.cluster.local:8080/v1",
    api_key="not-needed"  # LLMKube doesn't require auth yet
)

response = client.chat.completions.create(
    model="tinyllama",
    messages=[{"role": "user", "content": "Hello!"}]
)
print(response.choices[0].message.content)
```

**Node.js:**
```javascript
const { OpenAI } = require('openai');

const client = new OpenAI({
  baseURL: 'http://tinyllama-service.default.svc.cluster.local:8080/v1',
  apiKey: 'not-needed'
});

const response = await client.chat.completions.create({
  model: 'tinyllama',
  messages: [{ role: 'user', content: 'Hello!' }]
});

console.log(response.choices[0].message.content);
```

## Clean Up

```bash
# Delete inference service
kubectl delete inferenceservice tinyllama-service

# Delete model
kubectl delete model tinyllama

# Uninstall operator (optional)
kubectl delete namespace llmkube-system
kubectl delete crd models.inference.llmkube.dev
kubectl delete crd inferenceservices.inference.llmkube.dev
```

## Learn More

- **Documentation**: [README.md](../../README.md)
- **Roadmap**: [ROADMAP.md](../../ROADMAP.md)
- **Contributing**: [CONTRIBUTING.md](../../CONTRIBUTING.md)
- **More Examples**: [examples/](../)

## Support

- **Issues**: [GitHub Issues](https://github.com/Defilan/LLMKube/issues)
- **Discussions**: Coming soon
- **Community**: Discord/Slack (coming soon)

---

**Congratulations!** You've deployed your first LLM with LLMKube. 🚀

Time to first inference: **~5 minutes**

# GPU Quickstart Example

Deploy Llama 3.2 3B with GPU acceleration in under 2 minutes.

## Prerequisites

- Kubernetes cluster with GPU nodes (NVIDIA)
- NVIDIA GPU Operator installed
- kubectl configured
- LLMKube operator installed

## Quick Deploy

### Option 1: Using kubectl

```bash
# Deploy model and service
kubectl apply -f model.yaml
kubectl apply -f inferenceservice.yaml

# Wait for model to download (3.2GB, ~1-2 minutes)
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/llama-3b-gpu --timeout=300s

# Wait for service to be ready
kubectl wait --for=jsonpath='{.status.phase}'=Ready inferenceservice/llama-3b-gpu-service --timeout=600s

# Port forward to access the API
kubectl port-forward svc/llama-3b-gpu-service 8080:8080
```

### Option 2: Using LLMKube CLI (Recommended)

```bash
# Deploy with one command
llmkube deploy llama-3b-gpu --gpu \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf

# Check status
llmkube status llama-3b-gpu

# Port forward
kubectl port-forward svc/llama-3b-gpu-service 8080:8080
```

## Test Inference

Once the service is ready and port-forwarded, test it:

```bash
# Simple test
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "max_tokens": 50
  }'

# Longer conversation
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Explain Kubernetes in one sentence."}
    ],
    "max_tokens": 100,
    "temperature": 0.7
  }'

# Streaming response
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Count from 1 to 5"}],
    "max_tokens": 50,
    "stream": true
  }'
```

## Expected Performance

On NVIDIA L4 GPU:
- **Generation**: ~64 tokens/sec
- **Prompt Processing**: ~1,026 tokens/sec
- **Total Response Time**: ~0.6s (for 50-token response)
- **GPU Layers**: 29/29 (100% offloaded)
- **GPU Memory**: ~4.2GB VRAM
- **Power**: ~35W
- **Temperature**: 56-58°C

Performance will vary based on your GPU type:
- **T4**: ~40-50 tok/s generation
- **L4**: ~60-70 tok/s generation
- **A100**: ~100+ tok/s generation

## Verify GPU Usage

Check that the pod is using GPU:

```bash
# Get pod name
POD_NAME=$(kubectl get pods -l app=llama-3b-gpu-service -o jsonpath='{.items[0].metadata.name}')

# Check GPU resource allocation
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
# Should output: 1

# Check GPU layers argument
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].args}' | grep -o '\--n-gpu-layers [0-9]*'
# Should output: --n-gpu-layers 99 (or actual layer count)

# Check pod logs for GPU confirmation
kubectl logs $POD_NAME | grep -i "gpu\|cuda\|offload"
# Should show messages about GPU layers being offloaded
```

## Monitor GPU Metrics

If you have the observability stack installed:

```bash
# Port forward to Grafana
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Access Grafana at http://localhost:3000
# Default credentials: admin / prom-operator

# Import the GPU dashboard from config/grafana/llmkube-gpu-dashboard.json
```

## Cleanup

```bash
# Using kubectl
kubectl delete -f inferenceservice.yaml
kubectl delete -f model.yaml

# Using CLI
llmkube delete llama-3b-gpu
```

## Troubleshooting

### Pod not scheduling on GPU node

Check node labels and taints:

```bash
# List GPU nodes
kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-l4

# If no nodes found, check your node pool configuration
kubectl get nodes -o json | jq '.items[] | select(.status.capacity."nvidia.com/gpu" != null) | .metadata.name'
```

### Model download stuck

Check init container logs:

```bash
POD_NAME=$(kubectl get pods -l app=llama-3b-gpu-service -o jsonpath='{.items[0].metadata.name}')
kubectl logs $POD_NAME -c model-downloader
```

### GPU not being utilized

Check if GPU device plugin is running:

```bash
kubectl get pods -n kube-system | grep nvidia-device-plugin
```

### Low performance

Verify all layers are offloaded:

```bash
kubectl logs $POD_NAME | grep "llm_load_tensors"
# Look for "offloaded" count matching total layer count
```

## What's Happening Under the Hood

1. **Model CRD**: Defines the model source, GPU requirements, and resource needs
2. **InferenceService CRD**: Creates a Deployment with:
   - Init container to download model (3.2GB)
   - Main container running llama.cpp with CUDA
   - GPU resource requests (`nvidia.com/gpu: 1`)
   - GPU tolerations for tainted nodes
   - GPU layer offloading args (`--n-gpu-layers 99`)
3. **Automatic Scheduling**: Kubernetes schedules pod on GPU node
4. **Model Loading**: llama.cpp loads model and offloads layers to GPU
5. **Ready**: Service becomes available at OpenAI-compatible endpoint

## Next Steps

- **Scale up**: Increase `replicas` in `inferenceservice.yaml`
- **Larger models**: Try 7B or 13B models (adjust GPU memory accordingly)
- **Multi-GPU**: Set `gpu.count: 2` for models that need >24GB VRAM
- **Production**: Add resource limits, health checks, monitoring alerts

## Learn More

- [LLMKube Documentation](../../README.md)
- [GPU Performance Guide](../../docs/gpu-performance-phase0.md)
- [Observability Setup](../../config/prometheus/)
- [Full API Reference](../../api/v1alpha1/)

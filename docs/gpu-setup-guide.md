# GPU Setup and Verification Guide

This guide walks you through verifying GPU setup and deploying your first GPU-accelerated LLM.

## Prerequisites

- GKE cluster deployed via `terraform/gke/quick-start.sh`
- kubectl configured and connected
- Updated controller deployed with GPU support

## Step 1: Verify GPU Setup

### Check Cluster and GPU Nodes

```bash
# Check all nodes
kubectl get nodes -o wide

# Check GPU nodes specifically (may be 0 if auto-scaled down)
kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-tesla-t4

# Check GPU allocatable resources
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
```

### Verify NVIDIA Device Plugin

```bash
# Check device plugin daemonset
kubectl get daemonset -n kube-system | grep nvidia

# Check device plugin pods (only runs on GPU nodes)
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds -o wide

# Check logs
kubectl logs -n kube-system -l name=nvidia-device-plugin-ds --tail=50
```

### Test GPU with Simple Workload

```bash
# Deploy test pod
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: gpu-test
spec:
  restartPolicy: OnFailure
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

# Wait for pod (may take 2-3 min if node scaling from 0)
kubectl wait --for=condition=Ready pod/gpu-test --timeout=300s || echo "Pod may still be pending..."

# Check status
kubectl get pod gpu-test -o wide

# View GPU info
kubectl logs gpu-test

# Expected output: nvidia-smi showing 1x Tesla T4, ~15GB memory

# Clean up
kubectl delete pod gpu-test
```

**Troubleshooting:**
- If pod stays Pending: Check `kubectl describe pod gpu-test` for events
- If "Insufficient nvidia.com/gpu": GPU nodes may be scaling up (wait 2-3 min)
- If node selector doesn't match: Verify GPU node labels with `kubectl get nodes --show-labels`

## Step 2: Deploy Updated Controller

The controller now includes GPU scheduling logic. Rebuild and deploy:

```bash
# From project root
cd /Users/defilan/stuffy/code/ai/llmkube

# Regenerate CRDs with new GPU fields
make generate
make manifests

# Install updated CRDs
make install

# Build and push new controller image
make docker-build docker-push IMG=ghcr.io/defilan/llmkube-controller:v0.2.0

# Deploy updated controller
make deploy IMG=ghcr.io/defilan/llmkube-controller:v0.2.0

# Verify controller is running
kubectl get pods -n llmkube-system
kubectl logs -n llmkube-system deployment/llmkube-controller-manager --tail=50
```

**GPU Features Added:**
- ✅ Node selector for `nvidia-tesla-t4` nodes
- ✅ Tolerations for `nvidia.com/gpu` taint
- ✅ `--n-gpu-layers` flag for layer offloading
- ✅ Automatic GPU resource requests

## Step 3: Benchmark llama.cpp CUDA Backend

### Deploy a 7B Model with GPU

```bash
# Create a GPU-accelerated model
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: phi-3-7b-gpu
  namespace: default
spec:
  source: https://huggingface.co/microsoft/Phi-3-mini-4k-instruct-gguf/resolve/main/Phi-3-mini-4k-instruct-q4.gguf
  format: gguf
  quantization: Q4
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
      vendor: nvidia
      layers: -1  # Offload all layers to GPU
  resources:
    cpu: "4"
    memory: "8Gi"
EOF

# Create GPU inference service
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-3-7b-gpu-service
  namespace: default
spec:
  modelRef: phi-3-7b-gpu
  replicas: 1
  resources:
    gpu: 1
    gpuMemory: "16Gi"
    cpu: "2"
    memory: "4Gi"
  endpoint:
    port: 8080
    type: ClusterIP
EOF
```

### Monitor Deployment

```bash
# Watch model status
kubectl get model phi-3-7b-gpu -w

# Watch service status
kubectl get inferenceservice phi-3-7b-gpu-service -w

# Check pod scheduling (should land on GPU node)
kubectl get pods -l app=phi-3-7b-gpu-service -o wide

# View init container logs (model download)
POD=$(kubectl get pod -l app=phi-3-7b-gpu-service -o jsonpath='{.items[0].metadata.name}')
kubectl logs $POD -c model-downloader -f

# View main container logs (GPU detection)
kubectl logs $POD -c llama-server -f
```

**Expected GPU Logs:**
```
llama_model_load: loaded meta data with XX key-value pairs and XX tensors
llama_model_load: using CUDA backend
llm_load_tensors: offloading XX repeating layers to GPU
llm_load_tensors: offloaded XX/XX layers to GPU
llama_new_context_with_model: CUDA buffer size = 4096.00 MiB
```

### Run Benchmark Tests

```bash
# Port forward to service
kubectl port-forward svc/phi-3-7b-gpu-service 8080:8080 &

# Run benchmark script
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "phi-3-7b-gpu",
    "messages": [
      {"role": "user", "content": "Explain quantum computing in 100 words"}
    ],
    "max_tokens": 100,
    "stream": false
  }' | jq .

# Check response time and tokens/sec in logs
kubectl logs $POD -c llama-server --tail=20
```

**Benchmark Targets (T4 GPU):**
- **Prompt Processing**: >50 tok/s (vs ~29 CPU)
- **Token Generation**: >100 tok/s (vs ~18.5 CPU)
- **Latency P99**: <1s for simple queries
- **GPU Utilization**: Should see >80% during inference

### Monitor GPU Utilization

```bash
# SSH into GPU node (for debugging)
NODE=$(kubectl get pod $POD -o jsonpath='{.spec.nodeName}')
echo "Pod is running on node: $NODE"

# Deploy monitoring pod on same node
kubectl run gpu-monitor --rm -it --image=nvidia/cuda:12.2.0-base-ubuntu22.04 \
  --overrides='{"spec": {"nodeSelector": {"kubernetes.io/hostname": "'$NODE'"}, "tolerations": [{"key": "nvidia.com/gpu", "operator": "Exists"}]}}' \
  -- watch -n 1 nvidia-smi
```

## Step 4: Performance Comparison

### CPU Baseline (for comparison)

```bash
# Deploy same model on CPU
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-3-7b-cpu-service
spec:
  modelRef: phi-3-7b-gpu  # Same model, different service
  replicas: 1
  resources:
    cpu: "4"
    memory: "8Gi"
  endpoint:
    type: ClusterIP
EOF

# Test CPU performance
kubectl port-forward svc/phi-3-7b-cpu-service 8081:8080 &
time curl -X POST http://localhost:8081/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "Hello"}], "max_tokens": 50}'
```

### Expected Performance Improvements

| Metric | CPU (n2-standard-4) | GPU (T4) | Speedup |
|--------|---------------------|----------|---------|
| Prompt Processing | ~29 tok/s | >100 tok/s | 3-5x |
| Token Generation | ~18.5 tok/s | >100 tok/s | 5-10x |
| Cold Start | ~5s | ~8s | -1.6x |
| Latency P99 | ~2s | <1s | 2x |
| Cost/1K tokens | $0.002 | $0.008 | 4x more expensive |

**Cost Analysis:**
- GPU is 4x more expensive but 5-10x faster
- For production workloads with latency SLOs, GPU is cost-effective
- For batch/async workloads, CPU may be sufficient

## Step 5: Clean Up

```bash
# Delete inference services
kubectl delete inferenceservice phi-3-7b-gpu-service phi-3-7b-cpu-service

# Delete model
kubectl delete model phi-3-7b-gpu

# Stop port forwards
killall kubectl

# Optionally: Scale GPU nodes to 0 to save money
kubectl scale deployment -n kube-system --replicas=0 $(kubectl get deploy -n kube-system -o name | grep gpu)
```

## Next Steps

1. **Set up monitoring** (Phase 2-3): Prometheus + Grafana for GPU metrics
2. **CLI support** (Phase 2): Add `llmkube deploy --gpu 1` flag
3. **Multi-GPU** (Phase 4-5): Test 13B models on 2x T4 GPUs
4. **Optimize costs**: Implement auto-scaling based on traffic

## Troubleshooting

### Pod stuck in Pending
```bash
kubectl describe pod $POD
# Check for:
# - "Insufficient nvidia.com/gpu": Wait for node to scale up
# - "node(s) didn't match Pod's node affinity": GPU nodes not available
# - "taint node.kubernetes.io/not-ready": Node still initializing
```

### GPU not detected in llama.cpp
```bash
kubectl logs $POD -c llama-server | grep -i cuda
# Should see: "using CUDA backend"
# If not: Image may not have CUDA support, check image tag
```

### Poor GPU performance
```bash
# Check GPU utilization
kubectl exec -it $POD -- nvidia-smi
# Should show >80% GPU-Util during inference
# If low: Increase batch size or concurrent requests
```

### Out of GPU memory
```bash
kubectl logs $POD -c llama-server | grep -i "out of memory"
# Solution: Reduce layers offloaded or use smaller quantization
# Example: Set gpu.layers: 20 instead of -1 in Model spec
```

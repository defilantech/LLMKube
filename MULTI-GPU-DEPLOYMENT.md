# Multi-GPU Deployment Guide

Complete guide for deploying and testing multi-GPU support (Issue #2).

## Overview

This guide walks you through:
1. Deploying a GKE cluster with 2 GPUs per node
2. Building and deploying the multi-GPU controller
3. Testing with Llama 2 13B model
4. Validating performance (target: >40 tok/s)

**Estimated Time**: 30-45 minutes
**Estimated Cost**: ~$1-3 for testing session (with spot instances and teardown)

---

## Prerequisites

✅ **Required**:
- Google Cloud account with billing enabled
- `gcloud` CLI installed and authenticated
- `terraform` installed (v1.0+)
- `kubectl` installed
- `docker` installed (for building controller)

✅ **Recommended**:
- Familiarity with Kubernetes
- GCP quota for 2+ T4 or L4 GPUs in `us-west1` (best GPU availability)

---

## Step 1: Deploy GKE Cluster with Multi-GPU Support

### 1.1 Configure Terraform

```bash
cd terraform/gke

# Use the multi-GPU configuration
cp multi-gpu.tfvars terraform.tfvars

# Edit and set your project ID
nano terraform.tfvars
# Change: project_id = "YOUR_PROJECT_ID_HERE"
```

**Configuration Options**:

**Option A: 2x T4 GPUs** (Recommended - Cost-effective)
```hcl
gpu_type     = "nvidia-tesla-t4"
gpu_count    = 2
machine_type = "n1-standard-8"
use_spot     = true
```
- Cost: ~$0.70/hr per node
- Good for: Initial testing, 13B models

**Option B: 2x L4 GPUs** (Better Performance)
```hcl
gpu_type     = "nvidia-l4"
gpu_count    = 2
machine_type = "g2-standard-24"
use_spot     = true
```
- Cost: ~$1.40/hr per node
- Good for: Performance validation, 13B-70B models

### 1.2 Deploy Cluster

```bash
# Initialize Terraform
terraform init

# Review the plan
terraform plan
# Verify: gpu_count = 2, machine_type supports 2 GPUs

# Deploy (takes 10-15 minutes)
terraform apply

# Get credentials
eval $(terraform output -raw connect_command)
```

### 1.3 Verify GPU Setup

```bash
# Check nodes and GPU allocation
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
# Expected: 2 GPUs per GPU node (may be 0 if auto-scaled down)

# Check GPU device plugin
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# Test 2-GPU allocation
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: multi-gpu-test
spec:
  restartPolicy: OnFailure
  containers:
  - name: cuda-test
    image: nvidia/cuda:12.2.0-base-ubuntu22.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 2
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
  nodeSelector:
    cloud.google.com/gke-nodepool: gpu-pool
EOF

# Wait for pod (may take 2-3 min if node needs to scale up)
kubectl wait --for=condition=Ready pod/multi-gpu-test --timeout=300s || echo "Still pending..."

# Check GPU detection
kubectl logs multi-gpu-test
# Expected: nvidia-smi showing 2 GPUs

# Clean up
kubectl delete pod multi-gpu-test
```

**If GPUs not showing**: Node may be scaling up. Wait 2-3 minutes and check again.

---

## Step 2: Build and Deploy Multi-GPU Controller

### 2.1 Build Controller Image

```bash
cd /Users/defilan/stuffy/code/ai/llmkube

# Set your image name
export IMG=gcr.io/$(gcloud config get-value project)/llmkube-controller:v0.3.0-multi-gpu

# Build and push
make docker-build IMG=$IMG
make docker-push IMG=$IMG
```

### 2.2 Install CRDs and Deploy Controller

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=$IMG

# Verify controller is running
kubectl get pods -n llmkube-system
# Expected: llmkube-controller-manager-xxxxx   1/1   Running

# Check logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager --tail=50
```

---

## Step 3: Deploy Multi-GPU Model

### 3.1 Deploy Llama 2 13B with 2 GPUs

```bash
cd /Users/defilan/stuffy/code/ai/llmkube

# Deploy multi-GPU model
kubectl apply -f config/samples/multi-gpu-llama-13b-model.yaml

# Monitor deployment
kubectl get model llama-13b-multi-gpu -w
# Wait for: PHASE=Ready

kubectl get inferenceservice llama-13b-multi-gpu-service -w
# Wait for: PHASE=Ready
```

**Expected Timeline**:
- Model download: 2-5 minutes (7.4GB file)
- Pod startup: 1-2 minutes
- Model loading: 30-60 seconds
- **Total**: ~5-10 minutes

### 3.2 Verify Multi-GPU Configuration

```bash
# Get pod name
export POD=$(kubectl get pod -l app=llama-13b-multi-gpu-service -o jsonpath='{.items[0].metadata.name}')

# Verify 2 GPUs allocated
kubectl get pod $POD -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
# Expected: 2

# Check container args
kubectl get pod $POD -o jsonpath='{.spec.containers[*].args}' | tr ' ' '\n'
# Expected to see:
# --n-gpu-layers 99
# --split-mode layer
# --tensor-split 1,1

# Check pod logs for GPU detection
kubectl logs $POD | grep -i "gpu\|cuda\|offload\|split"
# Expected:
# - "using CUDA backend"
# - "n_split = 2"
# - "offloaded X/X layers to GPU"
```

**Example Log Output**:
```
llama_model_load: using CUDA backend
llm_load_tensors: offloading 40 repeating layers to GPU
llm_load_tensors: offloaded 40/40 layers to GPU
llama_new_context_with_model: split_mode = 1
llama_new_context_with_model: n_split = 2
```

---

## Step 4: Test Performance

### 4.1 Port Forward and Test

```bash
# Port forward
kubectl port-forward svc/llama-13b-multi-gpu-service 8080:8080 &

# Send test request
time curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Explain quantum computing in 100 words"}],
    "max_tokens": 100,
    "stream": false
  }' | jq -r '.choices[0].message.content'

# Check logs for performance metrics
kubectl logs $POD --tail=30 | grep "tokens per second"
```

### 4.2 Run Benchmark

```bash
# Run comprehensive benchmark
cd test/e2e

# Create benchmark script
cat > benchmark.sh <<'EOF'
#!/bin/bash
ENDPOINT="http://localhost:8080"
ITERATIONS=5

echo "Running $ITERATIONS iterations..."
for i in $(seq 1 $ITERATIONS); do
  echo "Iteration $i..."
  curl -s -w "\nTime: %{time_total}s\n" -X POST $ENDPOINT/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
      "messages": [{"role": "user", "content": "Explain machine learning"}],
      "max_tokens": 200
    }' | jq -r '.usage.completion_tokens, .usage.total_tokens'
  sleep 2
done
EOF

chmod +x benchmark.sh
./benchmark.sh
```

### 4.3 Monitor GPU Utilization

```bash
# Get node name
export NODE=$(kubectl get pod $POD -o jsonpath='{.spec.nodeName}')

# Run nvidia-smi monitoring
kubectl run gpu-monitor --rm -it \
  --image=nvidia/cuda:12.2.0-base-ubuntu22.04 \
  --overrides="{
    \"spec\": {
      \"nodeSelector\": {\"kubernetes.io/hostname\": \"$NODE\"},
      \"tolerations\": [{\"key\": \"nvidia.com/gpu\", \"operator\": \"Exists\"}]
    }
  }" \
  -- watch -n 1 nvidia-smi

# In another terminal, send requests
for i in {1..10}; do
  curl -X POST http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"messages": [{"role": "user", "content": "Count to 50"}], "max_tokens": 200}' &
done
```

**Expected GPU Utilization**:
- Both GPU0 and GPU1 should show 70-90% SM utilization
- Memory usage should be balanced across both GPUs
- Temperature: 55-75°C under load

---

## Step 5: Validate Success Criteria

### Success Checklist (from Issue #2)

- [ ] **Controller generates correct args**:
  ```bash
  kubectl get pod $POD -o jsonpath='{.spec.containers[*].args}' | grep tensor-split
  # Should see: --tensor-split 1,1
  ```

- [ ] **2 GPUs allocated**:
  ```bash
  kubectl get pod $POD -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
  # Should show: 2
  ```

- [ ] **Layers distributed across GPUs**:
  ```bash
  kubectl logs $POD | grep "offloaded"
  # Should see: "offloaded X/X layers to GPU"
  ```

- [ ] **Performance target met**:
  - Token generation: **>40 tok/s** (target from Issue #2)
  - Latency: <2s for 100-token completion

- [ ] **Both GPUs utilized**:
  - nvidia-smi shows >70% utilization on both GPUs during inference

---

## Step 6: Cleanup

### Save Costs!

**IMPORTANT**: GPU nodes are expensive. Always cleanup when done!

### Option A: Destroy Entire Cluster (Recommended)

```bash
cd terraform/gke
terraform destroy
# Type: yes
```

### Option B: Keep Cluster, Remove Workloads

```bash
# Delete inference service and model
kubectl delete inferenceservice llama-13b-multi-gpu-service
kubectl delete model llama-13b-multi-gpu

# Cluster will auto-scale GPU nodes to 0 (if min_gpu_nodes=0)
```

### Verify Cleanup

```bash
# Check GPU nodes are gone or scaled down
kubectl get nodes -l cloud.google.com/gke-nodepool=gpu-pool

# Verify in GCP Console
# https://console.cloud.google.com/kubernetes/list
```

---

## Troubleshooting

### Pod Stuck in Pending

```bash
kubectl describe pod $POD | grep -A 10 Events

# Common issues:
# - "Insufficient nvidia.com/gpu": Wait for node to scale up (2-3 min)
# - "node(s) didn't match": No nodes with 2 GPUs available
```

**Solution**: Check GPU quota and node pool configuration

### Layers Not Distributed

```bash
kubectl logs $POD | grep "offloaded"
# If showing 0 layers offloaded:
```

**Checks**:
1. Verify CUDA image: `kubectl get pod $POD -o jsonpath='{.spec.containers[0].image}'`
2. Check args: `kubectl get pod $POD -o jsonpath='{.spec.containers[*].args}'`
3. Verify GPU allocated: `kubectl describe pod $POD | grep nvidia.com/gpu`

### Poor Performance

If performance is below target (40 tok/s):

```bash
# Check actual GPU utilization
kubectl exec $POD -- nvidia-smi

# If one GPU at 100% and other low:
# - Tensor split may be imbalanced
# - Model may not support multi-GPU well
# - Check llama.cpp compatibility
```

### Out of Memory

```bash
kubectl logs $POD | grep -i "out of memory"

# Solution: Use smaller quantization
# Edit model.yaml: change Q8_0 -> Q4_K_M
```

---

## Next Steps

After successful validation:

1. **Document Performance**: Record tok/s, latency, GPU utilization
2. **Update Issue #2**: Add test results and mark success criteria as complete
3. **Test 4-GPU**: If cluster supports it, try 4-GPU configuration
4. **Update Roadmap**: Mark Phase 2-3 as complete
5. **Create PR**: Merge multi-GPU support to main branch

---

## Reference

- **Implementation**: `internal/controller/inferenceservice_controller.go`
- **Test Plan**: `test/e2e/multi-gpu-test-plan.md`
- **Examples**: `config/samples/multi-gpu-llama-13b-model.yaml`
- **Issue**: #2 - Multi-GPU single-node support
- **Roadmap**: Phase 2-3 (Multi-GPU & Platform Support)

---

## Support

If you encounter issues:

1. Check the [troubleshooting section](#troubleshooting) above
2. Review full test plan: `test/e2e/multi-gpu-test-plan.md`
3. File issue with logs: `kubectl logs $POD > pod-logs.txt`
4. Include GPU info: `kubectl exec $POD -- nvidia-smi > gpu-info.txt`

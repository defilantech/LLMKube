---
title: "Multi-GPU sharding"
description: "Shard one model across several GPUs on the same node with llama.cpp layer and tensor split: requirements, deploy commands, custom split ratios, and how to verify the split."
---

# Multi-GPU sharding

Running a model across two or more GPUs on a single node, using LLMKube's
layer-based sharding.

## Overview

This guide walks you through:
1. Deploying a GKE cluster with 2 GPUs per node
2. Installing the operator
3. Deploying Llama 2 13B across both GPUs
4. Confirming the layers actually landed on both

The GKE steps are one worked example. Multi-GPU sharding is cloud-agnostic:
anywhere a node advertises two or more `nvidia.com/gpu`, steps 2 onward apply
unchanged. `config/samples/` also ships AKS, EKS, and GKE spot variants.

**Estimated Time**: 30-45 minutes
**Estimated Cost**: ~$1-3 on GKE spot instances, if you tear down afterwards

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
cp multi-gpu.tfvars.example terraform.tfvars

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

## Step 2: Install the Operator

Multi-GPU sharding ships in the released operator; there is nothing special to
build for it.

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm install llmkube llmkube/llmkube --namespace llmkube-system --create-namespace

# Verify controller is running
kubectl get pods -n llmkube-system
# Expected: llmkube-controller-manager-xxxxx   1/1   Running

# Check logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager --tail=50
```

<details>
<summary>Building from source instead (contributors)</summary>

From a clone of the repo:

```bash
export IMG=gcr.io/$(gcloud config get-value project)/llmkube-controller:dev

make docker-build IMG=$IMG
make docker-push IMG=$IMG
make install          # CRDs
make deploy IMG=$IMG  # controller
```

</details>

---

## Step 3: Deploy Multi-GPU Model

### 3.1 Deploy Llama 2 13B with 2 GPUs

```bash
# Deploy multi-GPU model (from a clone of the repo, or use the raw URL)
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

### 4.2 Run Benchmark with LLMKube CLI

```bash
# Benchmark a single model
llmkube benchmark my-inference-service --iterations 10 --max-tokens 256

# Benchmark multiple models from the catalog with GPU support
llmkube benchmark --catalog llama-3.2-3b,mistral-7b,llama-3.1-8b \
  --gpu --gpu-count 2 \
  --iterations 10 --max-tokens 256 \
  --output markdown
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

## Step 5: Confirm the Shard Is Real

A model can be Ready, serving, and still running on one GPU. These five checks
distinguish "it works" from "it sharded".

- [ ] **Controller generated the sharding args**:
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

- [ ] **Throughput is in the expected range**: the reference figure recorded in
  `test/e2e/multi-gpu-test-plan.md` is above 40 tok/s for 13B on 2x L4. T4s are
  slower. Landing far below the figure for your hardware usually means one GPU
  is doing the work, so check the utilization item below before hunting
  elsewhere.

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

If throughput is well below the range in Step 5:

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

1. **Scale past 2 GPUs.** `hardware.gpu.count` is not limited to 2. Raise it
   and the controller recalculates `--tensor-split` for you.
2. **Try a larger model.** `config/samples/multi-gpu-llama-70b-model.yaml` is
   the 70B version of this walkthrough.
3. **Tune the split.** `hardware.gpu.sharding.strategy` accepts
   `layer;tensor;row;pipeline;none` and defaults to `layer`. Two of those are
   not what the name suggests: `tensor` is an alias for `row`, and `pipeline`
   falls back to `layer`, because llama.cpp has no pipeline split mode. Only
   `none` suppresses `--tensor-split` entirely. `layerSplit` takes explicit
   per-GPU ranges when the automatic even split is not what you want.
4. **Put it behind an SLO.** See the
   [GPU sharing guide](https://github.com/defilantech/LLMKube/blob/main/docs/operations/gpu-sharing.md)
   for quotas and `ModelPool`, and the [observability guide](/docs/operations/observability)
   for dashboards and SLOs, once more than one workload competes for the same
   GPUs.

---

## Reference

- **Implementation**: `internal/controller/gpu_sharding.go` (split mode and
  tensor-split calculation), `internal/controller/runtime_llamacpp.go` (arg
  construction)
- **Test plan**: `test/e2e/multi-gpu-test-plan.md`
- **Examples**: `config/samples/multi-gpu-llama-13b-model.yaml`,
  `multi-gpu-llama-70b-model.yaml`, plus `multi-gpu-{gke,eks,azure}-spot.yaml`
- **Terraform**: `terraform/gke/multi-gpu.tfvars.example` and
  `terraform/gke/multi-gpu-quick-start.sh`

---

## Support

If you encounter issues:

1. Check the [troubleshooting section](#troubleshooting) above
2. Review full test plan: `test/e2e/multi-gpu-test-plan.md`
3. File issue with logs: `kubectl logs $POD > pod-logs.txt`
4. Include GPU info: `kubectl exec $POD -- nvidia-smi > gpu-info.txt`

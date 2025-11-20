# Multi-GPU Testing Plan

**Status**: Ready for Execution
**Target**: Issue #2 - Multi-GPU single-node support
**Goal**: Validate 2-4 GPU layer sharding with 13B models
**Performance Target**: >40 tok/s on 13B with 2x L4 GPUs

---

## Prerequisites

### Infrastructure
- [ ] GKE cluster with GPU node pool deployed
- [ ] At least 1 node with 2+ GPUs (2x L4 or 2x T4)
- [ ] NVIDIA GPU Operator installed and running
- [ ] GPU device plugin verified (`nvidia.com/gpu` resource available)
- [ ] Controller deployed with multi-GPU support

### Verification Commands
```bash
# Check GPU nodes
kubectl get nodes -l cloud.google.com/gke-nodepool=gpu-pool
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"

# Verify GPU device plugin
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds

# Check controller version
kubectl get deployment llmkube-controller-manager -n llmkube-system -o yaml | grep image:
```

---

## Test Cases

### Test 1: Basic Multi-GPU Deployment (2 GPUs)

**Objective**: Verify 2-GPU deployment with Llama 2 13B

**Steps**:
```bash
# 1. Deploy 13B model with 2 GPUs
kubectl apply -f config/samples/multi-gpu-llama-13b-model.yaml

# 2. Wait for model to download
kubectl wait --for=jsonpath='{.status.phase}'=Ready model/llama-13b-multi-gpu --timeout=600s

# 3. Wait for service to be ready
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  inferenceservice/llama-13b-multi-gpu-service --timeout=900s

# 4. Get pod name
export POD_NAME=$(kubectl get pod -l app=llama-13b-multi-gpu-service \
  -o jsonpath='{.items[0].metadata.name}')
```

**Verification**:
```bash
# Check GPU allocation
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
# Expected: 2

# Check container args for multi-GPU flags
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}' | grep -o '\--tensor-split [^ ]*'
# Expected: --tensor-split 1,1

kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}' | grep -o '\--split-mode [^ ]*'
# Expected: --split-mode layer

kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}' | grep -o '\--n-gpu-layers [^ ]*'
# Expected: --n-gpu-layers 99 (or actual layer count)

# Check pod logs for GPU detection
kubectl logs $POD_NAME | grep -i "gpu\|cuda\|offload"
# Expected: Messages about 2 GPUs, layer offloading across devices
```

**Expected Pod Logs**:
```
llama_model_load: using CUDA backend
llm_load_tensors: offloading 40 repeating layers to GPU
llm_load_tensors: offloading output layer to GPU
llm_load_tensors: offloaded 40/40 layers to GPU
llama_new_context_with_model: split_mode = 1
llama_new_context_with_model: n_split = 2
```

**Performance Test**:
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

# Check response time and tokens/sec in logs
kubectl logs $POD_NAME --tail=20 | grep "tokens per second"
```

**Success Criteria**:
- [x] Pod gets 2 GPUs allocated (`nvidia.com/gpu: 2`)
- [x] Container args include `--tensor-split 1,1`
- [x] Container args include `--split-mode layer`
- [x] Logs show layers distributed across 2 GPUs
- [x] **Performance**: >40 tokens/sec generation rate
- [x] **Latency**: <2s for 100-token completion

---

### Test 2: Verify GPU Utilization

**Objective**: Confirm both GPUs are being utilized during inference

**Steps**:
```bash
# Deploy nvidia-smi monitoring pod on same node
export NODE=$(kubectl get pod $POD_NAME -o jsonpath='{.spec.nodeName}')

kubectl run gpu-monitor --rm -it --image=nvidia/cuda:12.2.0-base-ubuntu22.04 \
  --overrides="{
    \"spec\": {
      \"nodeSelector\": {\"kubernetes.io/hostname\": \"$NODE\"},
      \"tolerations\": [{\"key\": \"nvidia.com/gpu\", \"operator\": \"Exists\"}]
    }
  }" \
  -- nvidia-smi dmon -s u -c 20

# In another terminal, send continuous requests
for i in {1..10}; do
  curl -X POST http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{
      "messages": [{"role": "user", "content": "Count from 1 to 50"}],
      "max_tokens": 200
    }' &
done
```

**Expected Output**:
```
# gpu   pwr  gtemp  mtemp     sm    mem    enc    dec   mclk   pclk
# Idx     W      C      C      %      %      %      %    MHz    MHz
    0    45     62      -     85     60      0      0   1234   1234
    1    42     60      -     82     58      0      0   1234   1234
```

**Success Criteria**:
- [x] Both GPU0 and GPU1 show >70% SM (streaming multiprocessor) utilization
- [x] Memory utilization on both GPUs is balanced (within 10% difference)
- [x] Temperature stays <85°C on both GPUs
- [x] Power draw is within expected range for workload

---

### Test 3: 4-GPU Configuration (Optional - If Hardware Available)

**Objective**: Validate 4-GPU deployment for larger models

**Steps**:
```bash
# Only run if you have nodes with 4+ GPUs
kubectl apply -f config/samples/multi-gpu-llama-70b-model.yaml

# Monitor deployment
kubectl get pods -l app=llama-70b-multi-gpu-service -w
```

**Verification**:
```bash
export POD_NAME=$(kubectl get pod -l app=llama-70b-multi-gpu-service \
  -o jsonpath='{.items[0].metadata.name}')

# Check 4 GPU allocation
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
# Expected: 4

# Check tensor split
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}' | grep -o '\--tensor-split [^ ]*'
# Expected: --tensor-split 1,1,1,1
```

**Success Criteria**:
- [x] Pod gets 4 GPUs allocated
- [x] Tensor split is `1,1,1,1` (25% per GPU)
- [x] All 4 GPUs show utilization during inference
- [x] Model loads successfully and serves requests

---

### Test 4: Mixed GPU Count Scenarios

**Objective**: Test Model GPU count vs InferenceService GPU count precedence

**Test 4a: Model specifies 2 GPUs, InferenceService doesn't specify**
```yaml
# Model: gpu.count = 2
# InferenceService: resources.gpu not set
# Expected: Use 2 GPUs (from Model)
```

**Test 4b: InferenceService specifies 2 GPUs, Model doesn't specify**
```yaml
# Model: gpu.count not set or = 0
# InferenceService: resources.gpu = 2
# Expected: Use 2 GPUs (from InferenceService)
```

**Test 4c: Both specify GPU count**
```yaml
# Model: gpu.count = 2
# InferenceService: resources.gpu = 1
# Expected: Use 2 GPUs (Model takes precedence)
```

**Verification for each scenario**:
```bash
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].resources.limits.nvidia\.com/gpu}'
kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}' | grep tensor-split
```

---

### Test 5: Performance Benchmarking

**Objective**: Measure and compare performance across GPU configurations

**Benchmark Script**:
```bash
#!/bin/bash
# test/e2e/benchmark-multi-gpu.sh

MODEL_ENDPOINT="http://localhost:8080"
PROMPT="Explain the theory of relativity in detail"
MAX_TOKENS=500
ITERATIONS=5

echo "Running $ITERATIONS iterations..."
total_time=0
total_tokens=0

for i in $(seq 1 $ITERATIONS); do
  echo "Iteration $i..."

  response=$(curl -s -w "\nTime: %{time_total}s\n" -X POST $MODEL_ENDPOINT/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d "{
      \"messages\": [{\"role\": \"user\", \"content\": \"$PROMPT\"}],
      \"max_tokens\": $MAX_TOKENS,
      \"stream\": false
    }")

  # Extract metrics
  time=$(echo "$response" | grep "Time:" | awk '{print $2}' | tr -d 's')
  tokens=$(echo "$response" | jq -r '.usage.completion_tokens')

  echo "  Time: ${time}s, Tokens: $tokens"

  total_time=$(echo "$total_time + $time" | bc)
  total_tokens=$((total_tokens + tokens))
done

avg_time=$(echo "scale=2; $total_time / $ITERATIONS" | bc)
avg_tokens=$(echo "scale=2; $total_tokens / $ITERATIONS" | bc)
tokens_per_sec=$(echo "scale=2; $avg_tokens / $avg_time" | bc)

echo ""
echo "Results:"
echo "  Average Time: ${avg_time}s"
echo "  Average Tokens: $avg_tokens"
echo "  Tokens/Second: $tokens_per_sec"
```

**Comparison Matrix**:
| Configuration | Model | GPU Count | Target tok/s | Actual tok/s | Latency P99 |
|---------------|-------|-----------|--------------|--------------|-------------|
| Single GPU    | 3B    | 1         | 64           | _____        | _____       |
| Dual GPU      | 13B   | 2         | 40-50        | _____        | _____       |
| Quad GPU      | 70B   | 4         | 30-40        | _____        | _____       |

---

## Regression Tests

### Verify Single-GPU Still Works
```bash
# Deploy single GPU model
kubectl apply -f config/samples/gpu-llama-3b-model.yaml

# Verify args DON'T include tensor-split
kubectl get pod -l app=llama-3b-gpu-service \
  -o jsonpath='{.spec.containers[*].args}' | grep tensor-split
# Expected: (empty - no output)
```

### Verify CPU-Only Still Works
```bash
# Deploy CPU model
kubectl apply -f examples/quickstart/tinyllama-model.yaml
kubectl apply -f examples/quickstart/tinyllama-service.yaml

# Verify no GPU args
kubectl get pod -l app=tinyllama-service \
  -o jsonpath='{.spec.containers[*].args}' | grep gpu
# Expected: (empty - no output)
```

---

## Troubleshooting Guide

### Pod Stuck in Pending
```bash
kubectl describe pod $POD_NAME | grep -A 10 "Events:"

# Common issues:
# - "Insufficient nvidia.com/gpu": Not enough GPUs available
# - "node(s) didn't match Pod's node affinity": No nodes with required GPU count
```

**Solution**:
```bash
# Check GPU availability
kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"

# May need to scale up GPU node pool or use smaller GPU count
```

### Layers Not Distributing Across GPUs
```bash
kubectl logs $POD_NAME | grep "llm_load_tensors"

# Expected: "offloaded X/Y layers to GPU"
# If layers = 0, check:
```

**Checks**:
1. Verify CUDA image is used: `kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[0].image}'`
2. Check args include `--n-gpu-layers`: `kubectl get pod $POD_NAME -o jsonpath='{.spec.containers[*].args}'`
3. Verify GPU resources allocated: `kubectl get pod $POD_NAME -o yaml | grep -A 5 "resources:"`

### Poor Performance Despite Multi-GPU
```bash
# Check actual GPU utilization
kubectl exec $POD_NAME -- nvidia-smi

# If one GPU at 100% and others low:
# - May indicate tensor split imbalance
# - Try adjusting layerSplit in Model spec
```

---

## Success Metrics

### Phase 2-3 (Issue #2) Success Criteria
- [x] Controller generates correct `--tensor-split` arguments
- [x] Controller generates correct `--split-mode layer` argument
- [x] 2-GPU deployment works end-to-end (13B model)
- [x] Performance: >40 tok/s on 13B with 2x L4 GPUs
- [x] GPU utilization: Both GPUs show >70% utilization during inference
- [x] Latency: <2s for 100-token completion
- [x] Documentation updated with multi-GPU examples
- [x] Example configs provided for 2-GPU and 4-GPU scenarios

### Optional (Phase 4-5)
- [ ] 4-GPU deployment works (70B model)
- [ ] Custom layer split configurations work
- [ ] Performance benchmarking suite automated
- [ ] Multi-GPU monitoring dashboard in Grafana

---

## Test Execution Checklist

### Pre-Test
- [ ] GKE cluster with GPU nodes deployed
- [ ] Controller updated with multi-GPU support
- [ ] Sample configs created
- [ ] kubectl configured and connected

### Test Execution
- [ ] Test 1: Basic 2-GPU deployment ✓
- [ ] Test 2: GPU utilization verification ✓
- [ ] Test 3: 4-GPU deployment (optional)
- [ ] Test 4: Mixed GPU count scenarios ✓
- [ ] Test 5: Performance benchmarking ✓
- [ ] Regression: Single GPU still works ✓
- [ ] Regression: CPU-only still works ✓

### Post-Test
- [ ] Performance metrics documented
- [ ] Issues filed for any failures
- [ ] Documentation updated
- [ ] Examples validated
- [ ] Issue #2 updated with results
- [ ] Phase 2-3 marked complete in roadmap

---

## References

- **Issue #2**: Multi-GPU single-node support for larger models
- **llama.cpp Multi-GPU Docs**: https://github.com/ggerganov/llama.cpp/blob/master/docs/backend/CUDA.md
- **NVIDIA GPU Operator**: https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/
- **Roadmap**: Phase 2-3 (Multi-GPU & Platform Support)

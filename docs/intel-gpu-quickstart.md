# Intel GPU Quickstart

This guide walks through deploying an LLM on Intel GPUs with LLMKube.

## Prerequisites

- Kubernetes cluster with Intel GPU device plugin installed
- At least one node advertising Intel GPU resources
- LLMKube operator installed
- kubectl configured for your cluster

## Step 1: Verify Intel GPU Resources

Check allocatable Intel GPU resources on nodes:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,I915:.status.allocatable."gpu\.intel\.com/i915",XE:.status.allocatable."gpu\.intel\.com/xe"
```

Expected result: at least one of `gpu.intel.com/i915` or `gpu.intel.com/xe` is non-empty on a node.

## Step 2: Configure Resource Key (Optional)

By default, the controller uses `gpu.intel.com/i915` for Intel workloads.

If your cluster uses `gpu.intel.com/xe`, configure the controller:

```bash
kubectl -n llmkube-system set env deployment/llmkube-controller-manager LLMKUBE_INTEL_GPU_RESOURCE=gpu.intel.com/xe
kubectl -n llmkube-system rollout status deployment/llmkube-controller-manager
```

## Step 3: Deploy a Model with Intel Accelerator

### Option A: CLI

```bash
llmkube deploy phi-4-mini \
  --gpu \
  --gpu-count 1 \
  --accelerator intel
```

Notes:

- `--accelerator intel` sets model hardware accelerator to Intel.
- Default Intel runtime image is `ghcr.io/ggml-org/llama.cpp:server-intel`.

### Option B: Kubernetes YAML

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: intel-phi4
  namespace: default
spec:
  source: https://huggingface.co/microsoft/Phi-4-mini-instruct-gguf/resolve/main/Phi-4-mini-instruct-q4_0.gguf
  format: gguf
  hardware:
    accelerator: intel
    gpu:
      enabled: true
      count: 1
      vendor: intel
      layers: -1
  resources:
    cpu: "2"
    memory: "4Gi"
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: intel-phi4
  namespace: default
spec:
  modelRef: intel-phi4
  replicas: 1
  image: ghcr.io/ggml-org/llama.cpp:server-intel
  resources:
    gpu: 1
    cpu: "2"
    memory: "4Gi"
  podSecurityContext:
    runAsUser: 0
    runAsGroup: 0
    fsGroup: 0
```

Apply:

```bash
kubectl apply -f intel-phi4.yaml
```

## Step 4: Verify Scheduling and Offload

Watch resource state:

```bash
kubectl get model intel-phi4 -w
kubectl get inferenceservice intel-phi4 -w
kubectl get pods -l app=intel-phi4 -o wide
```

Check deployment resource limits and toleration key:

```bash
kubectl get deploy intel-phi4 -o jsonpath='{.spec.template.spec.containers[0].resources.limits}{"\n"}{.spec.template.spec.tolerations}{"\n"}'
```

Expected to include Intel GPU resource key (for example `gpu.intel.com/i915`).

Confirm SYCL backend and GPU offload in logs:

```bash
POD=$(kubectl get pod -l app=intel-phi4 -o jsonpath='{.items[0].metadata.name}')
kubectl logs "$POD" -c llama-server --tail=200
```

Expected log markers:

- `loaded SYCL backend`
- `using device SYCL0`
- `offloaded ... layers to GPU`

## Step 5: Measure Token Throughput

Port-forward and run a request:

```bash
kubectl port-forward svc/intel-phi4 8080:8080
```

In another terminal:

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "messages": [{"role": "user", "content": "Explain Kubernetes in one sentence."}],
    "max_tokens": 128,
    "temperature": 0.2,
    "stream": false
  }' | jq '.timings.prompt_per_second, .timings.predicted_per_second'
```

- `prompt_per_second` is prompt token throughput.
- `predicted_per_second` is generation throughput.

## Troubleshooting

### ErrImagePull for Intel runtime

Use:

```text
ghcr.io/ggml-org/llama.cpp:server-intel
```

If pulling private images from GHCR, configure `imagePullSecrets` on the controller or workload namespace.

### Init container permission denied on /models

If `model-downloader` fails with permission denied errors, set `podSecurityContext` on the InferenceService:

```yaml
podSecurityContext:
  runAsUser: 0
  runAsGroup: 0
  fsGroup: 0
```

### WaitingForGPU or Insufficient GPU events

- Verify node allocatable resource key matches controller configuration.
- If nodes expose `gpu.intel.com/xe`, set `LLMKUBE_INTEL_GPU_RESOURCE=gpu.intel.com/xe`.
- Check pod events:

```bash
kubectl describe pod -l app=intel-phi4
```
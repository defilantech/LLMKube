---
title: Karpenter GPU autoscaling
description: Use LLMKube with Karpenter to autoscale GPU nodes on demand. Covers NodePool setup, the do-not-disrupt annotation via podAnnotations, scale-to-zero with replicas 0, and common footguns.
---

# Karpenter GPU autoscaling

LLMKube's `InferenceService` CRD works with Karpenter out of the box.
Karpenter watches for unschedulable pods and provisions GPU nodes on
demand, then consolidates them back when the workload shrinks. The
operator does not need a special Karpenter integration: it schedules
pods with the right resource requests, tolerations, and annotations,
and Karpenter does the rest.

This guide covers the four pieces you need to wire together:

1. A Karpenter `NodePool` that targets GPU instances
2. The `karpenter.sh/do-not-disrupt` annotation via the existing
   `podAnnotations` passthrough
3. Scale-to-zero economics with `replicas: 0`
4. Common footguns and how to avoid them

## Prerequisites

- A Kubernetes cluster (v1.30+) with Karpenter installed and
  configured for your cloud provider
- LLMKube operator installed (see
  [Install in 5 minutes](/docs/getting-started))
- `kubectl` configured against your cluster

## Step 1: Create a GPU NodePool

Karpenter needs a `NodePool` that targets GPU instances and a
`NodeClass` that defines the cloud-specific configuration. The
example below uses AWS with `p4d.24xlarge` (4x NVIDIA A100 80 GB).
Adjust the instance type and NodeClass for your provider.

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata: { name: gpu-pool }
spec:
  template:
    spec:
      requirements:
        - key: node.kubernetes.io/instance-type
          operator: In
          values: ["p4d.24xlarge"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: gpu-nodeclass
      taints:
        - key: nvidia.com/gpu
          value: "true"
          effect: NoSchedule
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata: { name: gpu-nodeclass }
spec:
  amiSelectorTerms:
    - tags:
        karpenter.sh/discovery: llmkube-gpu
  role: arn:aws:iam::123456789012:role/karpenter-gpu-node-role
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: llmkube-gpu
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: llmkube-gpu
```

The `nvidia.com/gpu:NoSchedule` taint is important: it prevents
non-GPU workloads from landing on GPU nodes, which would waste
expensive capacity. LLMKube inference pods must carry the matching
toleration to schedule onto these nodes.

## Step 2: Deploy an InferenceService with GPU resources

The `InferenceService` needs three things to work with Karpenter:

- **Resource requests** that match the GPU node's capacity
- **Tolerations** for the GPU taint
- **NodeSelector** or affinity to target GPU nodes

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: { name: llama-3-8b }
spec:
  source: https://huggingface.co/bartowski/Llama-3.1-8B-Instruct-GGUF/resolve/main/Llama-3.1-8B-Instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    accelerator: cuda
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  runtime: llamacpp
  resources:
    limits:
      nvidia.com/gpu: "1"
    requests:
      nvidia.com/gpu: "1"
      memory: "16Gi"
      cpu: "4"
  tolerations:
    - key: nvidia.com/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  nodeSelector:
    nvidia.com/gpu.product: "NVIDIA-A100-SXM4-80GB"
```

When you apply this, Karpenter sees the unschedulable pod,
provisions a `p4d.24xlarge` node, and the pod schedules. The
InferenceService reaches `Ready` phase.

## Step 3: Protect pods from disruption during startup

While a model is still downloading and loading, Karpenter may try to
consolidate the node out from under it. To prevent that, set the
`karpenter.sh/do-not-disrupt` annotation on the inference pod through
LLMKube's `podAnnotations` passthrough, which merges the annotation
onto the pod's metadata:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  podAnnotations:
    karpenter.sh/do-not-disrupt: "true"
```

Karpenter will not disrupt or consolidate a node whose pods carry
this annotation, so the model can finish loading and serve without
the node being reclaimed. Remove the annotation once you want the
node to be eligible for consolidation again (for example, after the
service scales to zero), or leave it in place for a long-running
service that should never be interrupted.

## Step 4: Scale to zero

LLMKube supports `replicas: 0` on the `InferenceService`. When you
set this, the operator scales the Deployment to zero pods. Karpenter
then sees no pods requesting GPU resources and consolidates the
node back, freeing the expensive GPU capacity.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  replicas: 0
```

To bring the service back, set `replicas: 1` (or any positive
value). Karpenter provisions a new GPU node on demand.

### Scale-to-zero economics

The cost model is straightforward:

- **While running:** you pay for the GPU node (e.g., ~$32/hour for
  a `p4d.24xlarge` on AWS on-demand).
- **While at zero:** you pay nothing for the GPU node. Karpenter
  consolidates it back within its consolidation window (default
  5 minutes).
- **Startup cost:** the first request after scaling up pays the
  cold-start penalty (model download + load). This is typically
  30 to 90 seconds depending on model size and network speed.

For workloads with predictable idle periods (off-hours, weekends,
batch windows), scale-to-zero can save 50 to 80 percent of GPU
costs. For always-on workloads, the cold-start penalty may not be
worth it.

### Skip warmup for faster cold starts

When scaling from zero, the llama.cpp warmup pass adds latency to
the first request. Set `noWarmup: true` to skip it and reduce
cold-start time at the cost of slightly higher first-request
latency:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  replicas: 0
  noWarmup: true
```

## Common footguns

### Resource requests too low

If your `resources.requests` are too small, Karpenter may
provision a cheaper instance that does not have enough GPU memory
for the model. The pod will schedule but the runtime will fail
with an OOM. Always size `nvidia.com/gpu` and `memory` to match
the model's actual needs. A 70B model at Q4 needs roughly 40 GB of
GPU memory, so a single A100 80 GB is fine, but a 24 GB card will
not work.

### Missing toleration

Without the `nvidia.com/gpu:NoSchedule` toleration, the inference
pod cannot schedule onto the GPU node. Karpenter will keep
provisioning nodes but the pod will remain Pending. Check the pod
events:

```bash
kubectl describe pod -l inference.llmkube.dev/service=llama-3-8b
# look for: 0/1 nodes are available: 1 node(s) had taint
# nvidia.com/gpu that the pod didn't tolerate
```

### Karpenter consolidates too aggressively

Karpenter's default consolidation window is 5 minutes. If you
scale an InferenceService up and immediately scale it back down,
Karpenter may consolidate the node before the model finishes
loading. Set the `karpenter.sh/do-not-disrupt` annotation via
`podAnnotations` (see Step 3) so Karpenter leaves the node alone
while the model loads.

### NodeSelector mismatch

The `nodeSelector` must match the labels Karpenter applies to the
provisioned node. If you use `nvidia.com/gpu.product` as a
selector, confirm that the NVIDIA GPU Operator or your NodeClass
actually sets that label. A mismatch means the pod will never
schedule, and Karpenter will keep trying to provision nodes that
the pod cannot land on.

### Spot instances and model downloads

If you use spot instances for your NodePool, the model download
init container may be interrupted mid-download. The operator
retries the download on the next pod, but this wastes time and
spot credits. For large models (70B+), prefer on-demand for the
initial provisioning and spot only for steady-state serving.

### Multiple InferenceServices on one GPU node

A `p4d.24xlarge` has 4x A100 80 GB GPUs. If you request
`nvidia.com/gpu: "1"` per InferenceService, Karpenter can fit up
to four services on one node. However, each service also needs
system memory for the KV cache. If four services each request
16 Gi of memory, the node needs at least 64 Gi of system RAM
plus overhead. Check the node's total memory capacity before
packing services tightly.

## Troubleshooting

**Karpenter does not provision a node**
Check the Karpenter controller logs:

```bash
kubectl logs -n karpenter -l app.kubernetes.io/name=karpenter
```

Common causes: insufficient cloud provider quotas, missing
NodeClass, or the NodePool's `requirements` not matching any
available instance type.

**Pod stays Pending after node is provisioned**
The node exists but the pod does not schedule. Check the pod
events for taint or resource mismatches:

```bash
kubectl describe pod -l inference.llmkube.dev/service=llama-3-8b
```

**Node is not consolidated after scaling to zero**
Karpenter consolidates on a timer, not instantly. Wait up to
5 minutes (the default consolidation window). If the node still
persists, check whether another workload is using it:

```bash
kubectl get pods -o wide --field-selector spec.nodeName=<gpu-node-name>
```

## Reference

- [Karpenter documentation](https://karpenter.sh/)
- [Karpenter do-not-disrupt annotation](https://karpenter.sh/docs/concepts/nodeclaims/#do-not-disrupt)
- [LLMKube CRD reference](/docs/concepts/crds)
- [Air-gapped install](/docs/guides/air-gapped) for offline GPU
  clusters
- [Memory-pressure protection](/docs/memory-pressure-protection)
  for eviction tuning on GPU nodes
- [Metrics-driven autoscaling tutorial](/docs/guides/metrics-driven-autoscaling)
  for HPA-based replica scaling driven by LLMKube and DCGM metrics

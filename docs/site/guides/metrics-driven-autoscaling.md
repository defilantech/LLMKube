---
title: Metrics-driven autoscaling for InferenceService
description: Step-by-step tutorial for autoscaling an InferenceService with a HorizontalPodAutoscaler driven by LLMKube Prometheus metrics and GPU utilization, plus node autoscaling so scaled-out replicas land on additional GPU nodes.
---

# Metrics-driven autoscaling for InferenceService

This tutorial shows how to autoscale an `InferenceService` end to end:
a `HorizontalPodAutoscaler` drives the replica count from real
inference metrics, and a node autoscaler provisions GPU capacity so
the new replicas have somewhere to land.

The pieces you will wire together:

1. The native `spec.autoscaling` field on `InferenceService`, which
   causes the controller to create and manage an HPA for you.
2. The metrics you can scale on: the llama.cpp server metrics exposed
   on inference pods (e.g. `llamacpp:requests_processing`) and GPU
   utilization (DCGM) via `prometheus-adapter` or KEDA.
3. Node autoscaling: Karpenter, Cluster Autoscaler, or GKE Node
   Auto-Provisioning, so scaled-out replicas find GPU nodes.
4. A worked example from zero load to scaled-out, with YAML you can
   apply.

## Prerequisites

- A Kubernetes cluster (v1.30+) with the LLMKube operator installed
- An InferenceService already serving traffic (see [Getting
  started](/docs/getting-started))
- Prometheus scraping the LLMKube operator and the inference pods
- `kubectl` configured against your cluster
- A node autoscaler installed (Karpenter recommended; Cluster
  Autoscaler or GKE NAP also work — see [Node autoscaling](#step-4-node-autoscaling))

## Step 1: Use `spec.autoscaling` (recommended)

The simplest way to autoscale an `InferenceService` is to set
`spec.autoscaling` on the CRD. The controller reconciles this field
and creates (or updates) a managed `HorizontalPodAutoscaler` that
targets the inference Deployment.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  runtime: llamacpp
  autoscaling:
    maxReplicas: 5
    # minReplicas defaults to 1 when omitted
    # metrics defaults to llamacpp:requests_processing (avg target 2)
    # when omitted
```

That is all you need for a working HPA. The controller picks the
default metric for the runtime — `llamacpp:requests_processing` for
llamacpp — and sets a default average target of 2. Override either by
populating `spec.autoscaling.metrics`:

```yaml
spec:
  autoscaling:
    maxReplicas: 10
    metrics:
      - type: Pods
        name: llamacpp:requests_processing
        targetAverageValue: "4"
```

Valid metric `type` values are `Pods` and `Resource`. The controller
owns this HPA and points it at the inference Deployment it created; that
is an implementation detail. When you author or manage an autoscaler
yourself, always target the `InferenceService` (see below), never the
Deployment.

The managed HPA has the same name as the `InferenceService`
(`llama-3-8b` above). The `AutoscalingReady` condition on the
`InferenceService` reports its state:

| Status | Reason | Meaning |
|---|---|---|
| `True` | `HPAManaged` | The controller created or updated the HPA. |
| `False` | `HPANameTaken` | An HPA the controller did not create already has that name. The controller leaves it alone (no overwrite, no delete), emits a Warning event once, and rechecks about every 2 minutes. This reason also appears while a foreign Deployment refusal is in flight, because the stopped sequence cannot let the HPA reconciler report it itself. See [the pitfall below](#an-hpa-with-the-services-name-already-exists). |
| `False` | `DeploymentNameTaken` | A foreign Deployment holds the generated name, so the controller refused to provision the service's workload and removed the HPA it owns so the HPA cannot scale the foreign object (also while the service is suspended, where the HPA would have been removed anyway). The HPA (and this condition back to `True`) return when the name is free. See [the name-taken runbook](https://github.com/defilantech/LLMKube/blob/main/docs/operations/runbooks/inferenceservice-name-taken-deployment.md). |
| `False` | `HPAStateUnknown` | The controller could not read the HPA while refusing a foreign Deployment at the generated name; the reconcile retries in about 2 minutes. |
| absent | | Nothing to report: `spec.autoscaling` is not set, the service is suspended, or it is a Metal workload, and no Deployment-name refusal is in flight. |

```bash
kubectl get inferenceservice llama-3-8b \
  -o jsonpath='{.status.conditions[?(@.type=="AutoscalingReady")]}'
```

### Expose the metrics to the HPA

The llama.cpp server already emits colon-form names
(`llamacpp:requests_processing`, `llamacpp:prompt_tokens_total`, and so
on), which is exactly what the HPA metric refers to, so no renaming rule
is needed. vLLM and SGLang are the same: `vllm:num_requests_running` and
`sglang:num_running_reqs` are the names those servers publish.

What `prometheus-adapter` does provide is the `custom.metrics.k8s.io` API
itself. Without it the HPA has nowhere to read a Pods metric from and
reports `FailedGetPodsMetric`.

Expose the series as-is:

```yaml
rules:
  - seriesQuery: '{__name__=~"llamacpp:.+",namespace!="",pod!=""}'
    resources:
      overrides:
        namespace: {resource: namespace}
        pod: {resource: pod}
    name:
      matches: "^(.*)$"
      as: "$1"
    metricsQuery: sum(<<.Series>>{<<.LabelMatchers>>}) by (<<.GroupBy>>)
```

The managed `spec.autoscaling` path needs this too. The controller
writes the HPA object and fills in the runtime's default metric name;
it does not serve the metric, so `prometheus-adapter` (or another
custom-metrics API provider) still has to be installed and scraping
the inference pods.

### Hand-authored HPAs (advanced / custom metrics)

If you need a metric the controller does not support, or you want to
use a custom trigger (e.g. KEDA), you can author your own HPA and point
it at the `InferenceService` by name. The `InferenceService` implements
the `/scale` subresource, and the operator remains the single writer of
the backing Deployment's replica count.

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: llama-3-8b-hpa }
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llamacpp:requests_processing
        target:
          type: AverageValue
          averageValue: "4"
```

> **Warning:** do not point your HPA or KEDA `ScaledObject` at the inference
> Deployment. The operator owns the Deployment's replica count and rewrites
> `spec.replicas` on every reconcile, so an external write to the Deployment
> is reverted. Scale the `InferenceService` instead.
>
> Also do not configure both `spec.autoscaling` and a hand-authored HPA on
> the same `InferenceService`. Two controllers would fight over
> `spec.replicas` and you would get unpredictable scaling behavior. Pick one
> path and stick with it.

## Step 2: Pick your scaling signal

The right metric depends on what "load" means for your workload.

### Runtime default metrics

When `spec.autoscaling.metrics` is empty, the controller picks a sensible
default for each runtime backend:

| Runtime   | Default HPA metric           | Notes                                              |
|-----------|------------------------------|----------------------------------------------------|
| `llamacpp`| `llamacpp:requests_processing`| Requests currently in flight on the llama.cpp pod. |
| `vllm`    | `vllm:num_requests_running`   | Requests currently in flight on the vLLM pod.      |
| `tgi`     | `tgi:queue_size`              | See the TGI caveat below.                          |
| `sglang`  | `sglang:num_running_reqs`     | Requests currently in flight on the SGLang pod.    |

> **TGI caveat.** Unlike the other three, TGI does not publish a
> `tgi:queue_size` series; its own gauge is `tgi_batch_current_size`
> (which is what the drain-before-roll idle check reads). If you scale a
> TGI service on the default metric, add a `prometheus-adapter` rule that
> renames or computes `tgi:queue_size`, or set an explicit metric name in
> `spec.autoscaling.metrics[]`, otherwise the HPA reports
> `FailedGetPodsMetric`.

For SGLang, the controller also emits `--enable-metrics` and the
inference-pod template's port is named `http`, so the chart's
`inferencePodMonitor` scrapes `sglang:*` gauges out of the box (enable
with `prometheus.inferencePodMonitor.enabled=true`). Custom metrics work
the same way: write them into `spec.autoscaling.metrics` with `type:
Pods` and target the inferred Deployment.

### Option A: Requests being processed (simplest)

`llamacpp:requests_processing` is a gauge of how many requests the
llama.cpp server is currently processing. The controller uses it as
the default metric when `spec.autoscaling.metrics` is empty.

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llamacpp:requests_processing
        target:
          type: AverageValue
          averageValue: "4"   # scale up when avg > 4 in-flight reqs/pod
```

This is the lowest-friction signal. It scales on raw concurrency, not
quality. Good for batch-oriented workloads where you just need more
capacity.

### Option B: Deferred requests (saturation-driven)

`llamacpp:requests_deferred` is a gauge reporting how many requests
llama.cpp has deferred because every slot is busy. When this rises the
server is saturated and more replicas would help, so scaling out
before that point keeps throughput stable:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Pods
      pods:
        metric:
          name: llamacpp:requests_deferred
        target:
          type: AverageValue
          averageValue: "2"   # scale up when avg deferred requests > 2
```

Use this when your prompts vary in length and you want to avoid
cache thrash. Like Option A, this metric is served to the HPA through
the custom-metrics API by prometheus-adapter (see the adapter rule above).

### Option C: GPU utilization (hardware-driven)

For multi-tenant clusters or when you want to avoid GPU saturation,
use DCGM metrics via `prometheus-adapter`:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-3-8b-hpa
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 10
  metrics:
    - type: Pods
      pods:
        metric:
          name: DCGM_FI_DEV_GPU_UTIL
        target:
          type: AverageValue
          averageValue: "75"   # scale up when avg GPU util > 75%
```

`prometheus-adapter` exposes DCGM metrics as custom metrics the HPA
can read; like the llamacpp gauges above, `DCGM_FI_DEV_GPU_UTIL` needs
its own adapter rule before the HPA can resolve it. The NVIDIA DCGM
exporter must be running on each GPU node.
See the [NVIDIA GPU Operator docs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html)
for setup.

### Option D: KEDA scaler (advanced)

KEDA can scale on any Prometheus metric with more sophisticated
trigger shapes (e.g., scale to zero when metric is absent):

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: llama-3-8b-keda
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicaCount: 0
  maxReplicaCount: 10
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus:9090
        metricName: llamacpp_requests_processing
        query: |
          sum(llamacpp_requests_processing{namespace="default", pod=~"llama-3-8b.*"})
          /
          count(kube_pod_info{pod_label_inference_llmkube_dev_service="llama-3-8b"})
        threshold: "4"
```

KEDA manages the HPA underneath. The `scaleTargetRef` points at the
`InferenceService`, which implements the `/scale` subresource KEDA
requires: KEDA reads it for 0 to 1 activation, then delegates 1 to N to
the HPA it generates. Use this when you need scale-to-zero or complex
trigger logic.

## Step 3: Wire the scaler to the InferenceService

Apply the HPA from Step 2 (or the KEDA `ScaledObject` from Option D).
The HPA controller reads the `InferenceService`'s `/scale` subresource
and patches `spec.replicas` as needed:

```bash
kubectl apply -f hpa.yaml
```

Watch it work:

```bash
kubectl get hpa llama-3-8b-hpa -w
```

You should see `Current` and `Desired` replicas change as load
changes. `Current` reflects the observed inference pods the operator
reports, not the desired count. The HPA reconciles every 15 seconds by
default.

### Manual scaling

The same `/scale` subresource backs manual scaling. Scale the
`InferenceService`, never the Deployment:

```bash
kubectl scale inferenceservice/llama-3-8b --replicas 3
# or, with the CLI:
llmkube scale llama-3-8b --replicas 3
```

Writing `deployment.spec.replicas` directly does not work: the operator
is the single writer of the Deployment and reverts the count on its next
reconcile.

### Tuning the HPA

- **Stabilization window:** The HPA waits `behavior.stabilizationWindowSeconds`
  (default 300s) before scaling down. Reduce this for faster response
  to load drops:

  ```yaml
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
  ```

- **Cooldown:** Prevent flapping with a minimum time between scale
  events:

  ```yaml
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
      policies:
        - type: Percent
          value: 100
          periodSeconds: 120
  ```

- **Multiple metrics:** The HPA supports multiple metrics with
  different targets. Use `selectPolicy: Max` inside `behavior.scaleUp`
  to scale on the most aggressive metric:

  ```yaml
  spec:
    metrics:
      - type: Pods
        pods:
          metric:
            name: llamacpp:requests_processing
          target:
            type: AverageValue
            averageValue: "4"
      - type: Pods
        pods:
          metric:
            name: llamacpp:requests_deferred
          target:
            type: AverageValue
            averageValue: "2"
    behavior:
      scaleUp:
        selectPolicy: Max
  ```

## Step 4: Node autoscaling

When the HPA scales up replicas, the new pods need GPU nodes to
schedule on. If your cluster has no free GPU capacity, the pods will
stay Pending until you add nodes. Node autoscalers solve this.

### Karpenter (recommended)

Karpenter provisions GPU nodes on demand. See the [Karpenter GPU
autoscaling guide](/docs/guides/karpenter-gpu-autoscaling) for full
setup. The key piece for this tutorial is the `NodePool`:

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
```

Karpenter watches for unschedulable pods and provisions nodes. The
`nvidia.com/gpu:NoSchedule` taint prevents non-GPU workloads from
landing on GPU nodes.

### Cluster Autoscaler

The Kubernetes Cluster Autoscaler works with most cloud providers.
Configure it for GPU instance types:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler
  namespace: kube-system
data:
  config.yaml: |
    skip-nodes-with-local-storage: false
    expander: least-waste
    node-group-auto-discovery:
      asgDiscovery:
        region: us-west-2
        tag: k8s.io/cluster-autoscaler/enabled
    scale-down-utilization-threshold: 0.5
    scale-down-delay-after-add: 10m
```

The Cluster Autoscaler is simpler than Karpenter but slower to
provision nodes (typically 2-5 minutes vs. Karpenter's 30-60
seconds).

### GKE Node Auto-Provisioning

GKE has built-in node auto-provisioning. Enable it in your node pool:

```bash
gcloud container node-pools update default-pool \
  --cluster=my-cluster \
  --enable-autoprovisioning \
  --autoprovisioning-machine-types=a2-highgpu-1g \
  --autoprovisioning-min-cpu=4 \
  --autoprovisioning-min-memory=16 \
  --autoprovisioning-max-cpu=96 \
  --autoprovisioning-max-memory=640
```

GKE NAP is the lowest-friction option if you are on GKE. It
provisions nodes automatically based on resource requests.

## Step 5: Put it all together

Here is a complete example: an `InferenceService` with GPU resources
and tolerations, an HPA scaling on request rate, and a Karpenter
NodePool.

```yaml
# InferenceService
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: { name: llama-3-8b }
spec:
  modelRef: llama-3-8b
  runtime: llamacpp
  resources:
    gpu: 1
    cpu: "4"
    memory: "16Gi"
  tolerations:
    - key: nvidia.com/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  nodeSelector:
    nvidia.com/gpu.product: "NVIDIA-A100-SXM4-80GB"
---
# HPA
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: { name: llama-3-8b-hpa }
spec:
  scaleTargetRef:
    apiVersion: inference.llmkube.dev/v1alpha1
    kind: InferenceService
    name: llama-3-8b
  minReplicas: 1
  maxReplicas: 5
  metrics:
    - type: Pods
      pods:
        metric:
          name: llamacpp:requests_processing
        target:
          type: AverageValue
          averageValue: "4"
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 60
---
# Karpenter NodePool
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
```

Apply all three:

```bash
kubectl apply -f inference-service.yaml
kubectl apply -f hpa.yaml
kubectl apply -f nodepool.yaml
```

## Step 6: Verify end-to-end

1. **Check the InferenceService is ready:**

   ```bash
   kubectl get inferenceservice llama-3-8b
   # NAME          READY   REPLICAS
   # llama-3-8b    True    1
   ```

2. **Send load and watch the HPA scale up:**

   ```bash
   # Generate load (adjust URL to your service)
   kubectl run load-test --image=curlimages/curl:latest --restart=Never --rm -it -- \
     sh -c 'while true; do curl -s -X POST http://llama-3-8b:8080/v1/chat/completions \
       -H "Content-Type: application/json" \
       -d "{\"model\":\"llama-3-8b\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":4}"; sleep 1; done'

   # Watch the HPA
   kubectl get hpa llama-3-8b-hpa -w
   ```

   When you are done, delete the load pod so it stops sending requests:

   ```bash
   kubectl delete pod load-test
   ```

3. **Confirm nodes are provisioned:**

   ```bash
   kubectl get nodes -l nvidia.com/gpu=true
   # NAME                        STATUS   ROLES    AGE   VERSION
   # ip-10-0-1-1.us-west-2.compute.internal   Ready    <none>   2m    v1.30.0
   ```

4. **Stop the load and watch scale down:**

   ```bash
   kubectl delete pod load-test
   kubectl get hpa llama-3-8b-hpa -w
   ```

## Common pitfalls

### The scaler targets the Deployment

If the HPA shows `ScalingActive False` with reason `InvalidSelector`
("the HPA target's scale is missing a selector"), or replicas climb and
then snap back, the scaler is pointed at the inference Deployment
instead of the `InferenceService`. Repoint `scaleTargetRef` at
`inference.llmkube.dev/v1alpha1 InferenceService <name>`; the operator
reverts direct writes to the Deployment's replica count by design.

### An HPA with the service's name already exists

If `AutoscalingReady` is `False` with reason `HPANameTaken`, an HPA the
controller did not create (for example a hand-authored one, or one left
by another tool) already uses the name the controller generates, which is
the `InferenceService` name. The controller refuses to take it over: it
does not overwrite its spec, adopt it, or delete it when autoscaling is
cleared or the service is suspended. The rest of the service still
reconciles.

To fix it, either delete or rename the existing HPA (the controller
creates its own on the next check, within about 2 minutes), or remove
`spec.autoscaling` and keep the existing HPA as your hand-authored one.
Give hand-authored HPAs a different name from the service (the examples
here use `<service>-hpa`) so this cannot happen.

The refusal also applies while the service is suspended: the controller
does not delete an HPA it does not own. Suspension still scales the
Deployment to zero. If that HPA targets the inference Deployment rather
than the `InferenceService`, though, it fights the suspension: it scales
the Deployment back up and the controller forces it to zero again on the
next reconcile. Delete it before suspending.

This is the HPA-side refusal. A Deployment holding the generated name is
a separate refusal (`DeploymentNameTaken`) that stops the workload
provisioning itself; it is covered in
[the name-taken runbook](https://github.com/defilantech/LLMKube/blob/main/docs/operations/runbooks/inferenceservice-name-taken-deployment.md).

### HPA cannot read the metric

If the HPA shows `unknown` for current metrics, check:

- Prometheus is scraping the inference pods (check the
  `llamacpp:requests_processing` metric in the Prometheus UI)
- The metric name in the HPA matches the actual metric name
- `prometheus-adapter` is configured to expose custom metrics

### Pods stay Pending after scale up

The HPA increased replicas but pods cannot schedule. Check:

- GPU nodes exist (`kubectl get nodes -l nvidia.com/gpu=true`)
- The node has enough free GPU capacity
- The pod's tolerations match the node's taints
- Karpenter/Cluster Autoscaler is running and not blocked by
  quotas

### Scale thrashing

The HPA scales up and down rapidly. Fix:

- Increase `stabilizationWindowSeconds`
- Add a `scaleDown` policy with a minimum period
- Use a less sensitive metric (e.g., GPU utilization instead of
  request rate)

### Cold start latency

When scaling from zero, the first request pays the model load
penalty. Mitigate:

- Set `minReplicas: 1` to keep one pod warm
- Use `noWarmup: true` for faster cold starts
- Pre-warm with a synthetic request after scale up

## Reference

- [Karpenter GPU autoscaling](/docs/guides/karpenter-gpu-autoscaling)
  for node-level autoscaling
- [InferenceService CRD reference](/docs/concepts/crds) for the
  `spec.autoscaling` field and the `InferenceResourceRequirements`
  schema
- [KEDA documentation](https://keda.sh/) for advanced scaling
- [NVIDIA DCGM exporter](https://github.com/NVIDIA/dcgm-exporter)
  for GPU utilization metrics

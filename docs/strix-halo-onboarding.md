# Strix Halo Quickstart

This guide walks through onboarding an AMD Strix Halo (Ryzen AI Max+ 395, gfx1151) node into an LLMKube cluster and running an LLM on the integrated GPU.

## Prerequisites

- Strix Halo node with at least 4 GB UMA reservation in BIOS
- Linux kernel ≥ 6.15 (6.18.4+ recommended for gfx1151 stability)
- `amdgpu` kernel module loaded
- LLMKube operator installed
- kubectl configured for your cluster

## Step 1: Verify Host Prerequisites

Check the kernel version and that `amdgpu` is loaded:

```bash
uname -r
lsmod | grep amdgpu
ls -la /dev/dri/
```

Expected: kernel ≥ 6.15, `amdgpu` listed, and at least `renderD128` present.

If kernel args aren't yet applied, add them to your bootloader. Strix Halo requires:

```
amd_iommu=off
amdgpu.gttsize=126976
ttm.pages_limit=32505856
ttm.page_pool_size=25165824
amdgpu.vm_fragment_size=8
amdgpu.lockup_timeout=20000
```

The first five tune the GTT/UMA memory ceiling. `amdgpu.lockup_timeout=20000`
raises the GPU lockup/reset timeout to 20s (from the ~10s default): under
sustained heavy inference on gfx1151 (long-context coder runs, draft/MTP
decode) the default timeout can trip a spurious GPU reset ("device lost") that
kills the llama-server backend mid-run. 20s avoids the false positive without
masking a genuine hang.

Reboot after applying. Confirm they're live:

```bash
cat /proc/cmdline | tr ' ' '\n' | grep -E 'gttsize|ttm|amd_iommu|lockup_timeout'
```

For Talos nodes, set these in the machine config `customization.extraKernelArgs` and include the `siderolabs/amdgpu` and `siderolabs/amd-ucode` system extensions. See the [Talos AMD GPU guide](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/hardware-and-drivers/amd-gpu) for the full Talos recipe.

## Step 2: Install the ROCm GPU Operator

The [ROCm GPU Operator](https://instinct.docs.amd.com/projects/gpu-operator/en/latest/index.html) is the recommended way to expose AMD GPUs to Kubernetes. It bundles the device plugin, Node Feature Discovery rules, and (optionally) a metrics exporter.

Add the Helm repo and install:

```bash
helm repo add rocm https://rocm.github.io/gpu-operator
helm repo update
helm install rocm-gpu-operator rocm/gpu-operator \
  --namespace kube-system --create-namespace
```

Strix Halo is integrated, not discrete, so disable the operator's in-cluster driver loading (the kernel module is already loaded by the host):

```yaml
# values.yaml
kmm:
  enabled: true
installdefaultNFDRule: true
deviceConfig:
  spec:
    driver:
      enable: false
      blacklist: false
metricsExporter:
  enable: true
  prometheus:
    serviceMonitor:
      enable: true
      interval: 30s
```

For Strix Halo, the default NFD rules detect the iGPU by device ID and miss it. Add a kernel-module-based rule instead:

```yaml
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeatureRule
metadata:
  name: amdgpu-rules
spec:
  rules:
    - name: "amdgpu kernel module"
      labels:
        feature.node.kubernetes.io/amd-gpu: "true"
      matchFeatures:
        - feature: "kernel.loadedmodule"
          matchExpressions:
            amdgpu: {op: Exists}
```

This labels any node with the `amdgpu` module loaded with `feature.node.kubernetes.io/amd-gpu: "true"`, which the ROCm operator matches on.

For a deeper Strix Halo walkthrough including a PyTorch verification job, see the [strixhalo.wiki Kubernetes guide](https://strixhalo.wiki/Guides/Kubernetes).

### Alternative: rocm/k8s-device-plugin

For a lighter setup (no operator, no metrics exporter), use the upstream `rocm/k8s-device-plugin` image directly. NFD is still useful here for labeling Strix Halo nodes with `feature.node.kubernetes.io/amd-gpu: "true"` (the device plugin uses this label to target AMD nodes), so install NFD and apply the kernel-module-based rule from the operator section above even if you skip the operator itself. Example DaemonSet:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: amd-gpu-device-plugin
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/component: amd-gpu-device-plugin
  template:
    metadata:
      labels:
        app.kubernetes.io/component: amd-gpu-device-plugin
    spec:
      nodeSelector:
        feature.node.kubernetes.io/amd-gpu: "true"
      priorityClassName: system-node-critical
      containers:
      - name: amd-gpu-device-plugin
        image: docker.io/rocm/k8s-device-plugin:1.31.0.10
        resources:
          requests:
            cpu: 10m
            memory: 32Mi
          limits:
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: dev-dri
          mountPath: /dev/dri
          readOnly: true
        - name: sys
          mountPath: /sys
          readOnly: true
        - name: device-plugins
          mountPath: /var/lib/kubelet/device-plugins
      volumes:
      - name: dev-dri
        hostPath:
          path: /dev/dri
      - name: sys
        hostPath:
          path: /sys
      - name: device-plugins
        hostPath:
          path: /var/lib/kubelet/device-plugins
```

Apply:

```bash
kubectl apply -f amd-gpu-device-plugin.yaml
```

This image advertises `amd.com/gpu` for every AMD render node it sees. Add your own nodeSelector or taints to restrict scheduling.

## Step 3: Verify GPU Resources

Check that nodes now advertise the AMD GPU resource:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,AMD:.status.allocatable."amd\.com/gpu"
```

Expected: at least one node shows a non-zero `amd.com/gpu` value.

Confirm the NFD label is applied:

```bash
kubectl get nodes --show-labels | grep amd-gpu
```

## Step 4: Deploy a Model

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: phi-4-strix
  namespace: default
spec:
  source: https://huggingface.co/microsoft/Phi-4-mini-instruct-gguf/resolve/main/Phi-4-mini-instruct-q4_0.gguf
  format: gguf
  hardware:
    accelerator: amd
    gpu:
      enabled: true
      count: 1
      vendor: amd
      layers: -1
  resources:
    cpu: "2"
    memory: "4Gi"
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: phi-4-strix
  namespace: default
spec:
  modelRef: phi-4-strix
  replicas: 1
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
kubectl apply -f phi-4-strix.yaml
```

## Step 5: Verify Scheduling and Offload

Watch resource state:

```bash
kubectl get model phi-4-strix -w
kubectl get inferenceservice phi-4-strix -w
kubectl get pods -l app=phi-4-strix -o wide
```

Check the pod landed on a Strix Halo node and got `amd.com/gpu`:

```bash
kubectl get deploy phi-4-strix -o jsonpath='{.spec.template.spec.containers[0].resources.limits}{"\n"}{.spec.template.spec.tolerations}{"\n"}'
```

Confirm GPU offload in logs:

```bash
POD=$(kubectl get pod -l app=phi-4-strix -o jsonpath='{.items[0].metadata.name}')
kubectl logs "$POD" -c llama-server --tail=200
```

Expected log markers:

- `loaded Vulkan0 backend` (or `ROCm` if using a ROCm-tier image)
- `using device Vulkan0`
- `offloaded ... layers to GPU`

## Step 6: Measure Token Throughput

Port-forward and run a request:

```bash
kubectl port-forward svc/phi-4-strix 8080:8080
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

## Troubleshooting

### renderD128 not present

The `amdgpu` module isn't loaded or the kernel is too old. Verify:

```bash
lsmod | grep amdgpu
dmesg | grep amdgpu
```

### Insufficient amd.com/gpu

Device plugin or operator not running on the Strix Halo node. Check pod logs:

```bash
kubectl logs -n kube-system -l app.kubernetes.io/component=amd-gpu-device-plugin
# or
kubectl logs -n kube-system deployment/rocm-gpu-operator
```

### NFD label missing

The operator uses `feature.node.kubernetes.io/amd-gpu: "true"` to discover nodes. If the default NFD rule misses Strix Halo (it often does for iGPUs), apply the kernel-module-based rule shown in Step 2.

### GTT ceiling too low

Kernel args not applied. Confirm they're in `/proc/cmdline`:

```bash
cat /proc/cmdline | tr ' ' '\n' | grep -E 'gttsize|ttm|amd_iommu'
```

### OOM under load

The amdgpu GTT pins system RAM the kernel OOM-killer can't reclaim. On UMA nodes running multiple GPU pods, the heaviest pod can deadlock the node under sustained memory pressure. Configure a userspace OOM manager to kill the largest pod (by current memory usage) under sustained memory PSI pressure, while keeping system and runtime classes (kubelet, containerd) protected.

### GPU reset / "device lost" under sustained load

A spurious amdgpu reset (ring timeout, "device lost") kills the inference backend mid-run on long or draft/MTP-heavy workloads. Raise the GPU lockup timeout with `amdgpu.lockup_timeout=20000` (see [Step 1](#step-1-verify-host-prerequisites)) and reboot. Confirm it's live:

```bash
cat /proc/cmdline | tr ' ' '\n' | grep lockup_timeout
```

If resets persist after the bump, the workload is genuinely hanging the GPU rather than hitting a false timeout; check `dmesg | grep -i amdgpu` for the failing ring and reduce context length or draft depth.

## See also

- [Intel GPU quickstart](intel-gpu-quickstart.md) — same pattern, different vendor
- [NVIDIA GPU setup guide](gpu-setup-guide.md) — GKE-specific NVIDIA flow
- [Talos AMD GPU guide](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/hardware-and-drivers/amd-gpu) — Talos-specific kernel args and extensions
- [strixhalo.wiki Kubernetes guide](https://strixhalo.wiki/Guides/Kubernetes) — full Strix Halo k8s walkthrough with PyTorch verification
- [kyuz0/amd-strix-halo-toolboxes](https://github.com/kyuz0/amd-strix-halo-toolboxes) — reference container images and host tuning

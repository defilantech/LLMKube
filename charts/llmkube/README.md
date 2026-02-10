# LLMKube Helm Chart

A Helm chart for deploying LLMKube - a Kubernetes operator for GPU-accelerated LLM inference.

## Introduction

LLMKube is a Kubernetes operator that makes it easy to deploy, manage, and scale GPU-accelerated LLM inference services. Built for air-gapped environments, edge computing, and production workloads with first-class GPU support.

## Prerequisites

- Kubernetes 1.11.3+
- Helm 3.0+
- (Optional) Prometheus Operator for metrics and alerts
- (Optional) NVIDIA GPU Operator for GPU support

## Installing the Chart

### Add the Helm Repository

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update

# Install the chart
helm install llmkube llmkube/llmkube \
  --namespace llmkube-system \
  --create-namespace
```

### Install from Local Chart

```bash
# Clone the repository
git clone https://github.com/defilantech/LLMKube.git
cd LLMKube

# Install the chart
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace
```

### Install with Custom Values

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --set controllerManager.image.tag=0.2.1 \
  --set prometheus.serviceMonitor.enabled=true
```

### Install with Prometheus Integration

```bash
# Enable Prometheus ServiceMonitor and PrometheusRule
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --set prometheus.serviceMonitor.enabled=true \
  --set prometheus.prometheusRule.enabled=true \
  --set prometheus.prometheusRule.namespace=monitoring
```

## Uninstalling the Chart

```bash
helm uninstall llmkube --namespace llmkube-system
```

**Note:** By default, CRDs are kept after uninstallation to prevent data loss. To remove CRDs:

```bash
kubectl delete crd models.inference.llmkube.dev
kubectl delete crd inferenceservices.inference.llmkube.dev
```

## Configuration

The following table lists the configurable parameters of the LLMKube chart and their default values.

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `namespace` | Namespace to deploy the controller | `llmkube-system` |
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full chart name | `""` |

### Controller Manager Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `controllerManager.image.repository` | Controller image repository | `ghcr.io/defilantech/llmkube-controller` |
| `controllerManager.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `controllerManager.image.tag` | Image tag (defaults to chart appVersion) | `""` |
| `controllerManager.replicaCount` | Number of controller replicas | `1` |
| `controllerManager.leaderElection.enabled` | Enable leader election | `true` |
| `controllerManager.resources.limits.cpu` | CPU limit | `500m` |
| `controllerManager.resources.limits.memory` | Memory limit | `2Gi` |
| `controllerManager.resources.requests.cpu` | CPU request | `10m` |
| `controllerManager.resources.requests.memory` | Memory request | `512Mi` |
| `controllerManager.nodeSelector` | Node selector | `{}` |
| `controllerManager.tolerations` | Tolerations | `[]` |
| `controllerManager.affinity` | Affinity rules | `{}` |

### RBAC Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `rbac.create` | Create RBAC resources | `true` |
| `serviceAccount.create` | Create service account | `true` |
| `serviceAccount.name` | Service account name (auto-generated if empty) | `""` |
| `serviceAccount.annotations` | Service account annotations | `{}` |

### Metrics Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `metrics.enabled` | Enable metrics endpoint | `true` |
| `metrics.service.type` | Metrics service type | `ClusterIP` |
| `metrics.service.port` | Metrics service port | `8443` |

### Prometheus Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `prometheus.serviceMonitor.enabled` | Enable Prometheus ServiceMonitor | `false` |
| `prometheus.serviceMonitor.interval` | Scrape interval | `30s` |
| `prometheus.serviceMonitor.namespace` | ServiceMonitor namespace (defaults to release namespace) | `""` |
| `prometheus.serviceMonitor.additionalLabels` | Additional labels for ServiceMonitor | See values.yaml |
| `prometheus.prometheusRule.enabled` | Enable PrometheusRule for alerts | `false` |
| `prometheus.prometheusRule.namespace` | PrometheusRule namespace | `monitoring` |
| `prometheus.prometheusRule.rules.gpu.enabled` | Enable GPU alerts | `true` |
| `prometheus.prometheusRule.rules.gpu.highUtilizationThreshold` | GPU high utilization threshold (%) | `90` |
| `prometheus.prometheusRule.rules.gpu.highTemperatureThreshold` | GPU high temperature threshold (°C) | `85` |
| `prometheus.prometheusRule.rules.gpu.memoryPressureThreshold` | GPU memory pressure threshold (%) | `90` |
| `prometheus.prometheusRule.rules.gpu.powerLimitThreshold` | GPU power limit threshold (W) | `250` |
| `prometheus.prometheusRule.rules.inference.enabled` | Enable inference alerts | `true` |

### CRD Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `crds.install` | Install CRDs as part of chart | `true` |
| `crds.keep` | Keep CRDs on uninstall | `true` |

## Examples

### Basic Installation

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace
```

### Production Installation with Monitoring

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --values - <<EOF
controllerManager:
  replicaCount: 1
  resources:
    limits:
      cpu: 1
      memory: 4Gi
    requests:
      cpu: 100m
      memory: 1Gi

prometheus:
  serviceMonitor:
    enabled: true
    interval: 30s
  prometheusRule:
    enabled: true
    namespace: monitoring
    rules:
      gpu:
        enabled: true
        highUtilizationThreshold: 85
      inference:
        enabled: true
EOF
```

### Install Without CRDs

If CRDs are already installed or managed separately:

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --set crds.install=false
```

### Custom Image Repository

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --set controllerManager.image.repository=my-registry.io/llmkube-controller \
  --set controllerManager.image.tag=custom-tag
```

## Upgrading

### To Upgrade the Chart

```bash
helm upgrade llmkube charts/llmkube \
  --namespace llmkube-system
```

### To Upgrade with New Values

```bash
helm upgrade llmkube charts/llmkube \
  --namespace llmkube-system \
  --reuse-values \
  --set controllerManager.image.tag=0.2.1
```

## Deploying Models

After installing the chart, you can deploy models using:

### Using the CLI

```bash
# Install the CLI
brew tap defilantech/tap
brew install llmkube

# Deploy a model
llmkube deploy tinyllama \
  --source https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf \
  --cpu 500m \
  --memory 1Gi
```

### Using kubectl

```bash
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
  namespace: default
spec:
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf
  format: gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama
  namespace: default
spec:
  modelRef: tinyllama
  replicas: 1
  resources:
    cpu: "500m"
    memory: "1Gi"
EOF
```

## GPU Support

For GPU-accelerated inference, ensure:

1. NVIDIA GPU Operator is installed in your cluster
2. GPU nodes are available
3. Deploy with GPU configuration:

```bash
llmkube deploy llama-3b-gpu \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf \
  --gpu \
  --gpu-count 1 \
  --gpu-memory 8Gi
```

See the [GPU Setup Guide](https://github.com/defilantech/LLMKube/blob/main/docs/gpu-setup-guide.md) for detailed instructions.

## Troubleshooting

### Controller Not Starting

```bash
# Check controller logs
kubectl logs -n llmkube-system deployment/llmkube-controller-manager -f

# Check controller pod status
kubectl get pods -n llmkube-system
kubectl describe pod -n llmkube-system <pod-name>
```

### CRDs Not Installing

```bash
# Verify CRDs are installed
kubectl get crds | grep llmkube

# Manually install CRDs if needed
kubectl apply -f charts/llmkube/templates/crds/
```

### Prometheus Metrics Not Available

```bash
# Verify ServiceMonitor is created
kubectl get servicemonitor -n llmkube-system

# Check if prometheus-operator is installed
kubectl get pods -n monitoring | grep prometheus-operator
```

## Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](https://github.com/defilantech/LLMKube/blob/main/CONTRIBUTING.md) for details.

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0.

## Resources

- [GitHub Repository](https://github.com/defilantech/LLMKube)
- [Documentation](https://github.com/defilantech/LLMKube/tree/main/docs)
- [Issue Tracker](https://github.com/defilantech/LLMKube/issues)
- [Roadmap](https://github.com/defilantech/LLMKube/blob/main/ROADMAP.md)

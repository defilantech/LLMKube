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

### Install from the OCI Registry

```bash
helm install llmkube oci://ghcr.io/defilantech/charts/llmkube \
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
| `controllerManager.image.registry` | Image registry prefix (prepended to repository when set) | `""` |
| `controllerManager.image.repository` | Controller image repository | `ghcr.io/defilantech/llmkube-controller` |
| `controllerManager.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `controllerManager.image.tag` | Image tag (defaults to chart appVersion) | `""` |
| `controllerManager.image.digest` | Image digest (takes precedence over tag) | `""` |
| `controllerManager.initContainer.registry` | Init container image registry prefix | `""` |
| `controllerManager.initContainer.repository` | Init container image repository | `docker.io/curlimages/curl` |
| `controllerManager.initContainer.tag` | Init container image tag | `8.18.0` |
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

#### Scrape labels

Both monitors pin the `job` label so the chart's own alerts can select their
targets: the ServiceMonitor sets `job="llmkube-controller"` and the inference
PodMonitor sets `job="llmkube-inference"`. Left to the Prometheus Operator's
defaults these come out as the metrics Service name and
`<namespace>/<podmonitor name>` — which is what `ControllerDown` and
`InferenceServiceDown` used to fail to match. Queries of your own written
against those older values need updating.

The PodMonitor additionally promotes `service`, `namespace`, `model` and
`runtime` onto every inference series.

### Grafana Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `grafana.dashboards.enabled` | Ship the dashboards in `dashboards/` as a ConfigMap | `false` |
| `grafana.dashboards.only` | Dashboards to ship, by filename without `.json`. Empty ships all of them | `[]` |
| `grafana.dashboards.additionalLabels` | Additional labels for the dashboards ConfigMap | `{grafana_dashboard: "true"}` |
| `grafana.dashboards.namespace` | ConfigMap/CR namespace (defaults to release namespace) | `""` |
| `grafana.dashboards.annotations` | Annotations for the dashboards ConfigMap | `{}` |
| `grafana.dashboards.operator.enabled` | Also emit grafana-operator `GrafanaDashboard` CRs | `false` |
| `grafana.dashboards.operator.mode` | `auto` publishes runtime dashboards only while that runtime serves; `all` publishes every dashboard | `auto` |
| `grafana.dashboards.operator.instanceSelector` | `GrafanaDashboard` instance selector (required by the CRD) | `{}` |
| `grafana.dashboards.operator.allowCrossNamespaceImport` | Match Grafanas outside the CR's namespace | `true` |
| `grafana.dashboards.operator.folder` | Grafana folder for the dashboards (sidecar path uses the `grafana_folder` annotation) | `""` |
| `grafana.dashboards.operator.resyncPeriod` | How often grafana-operator re-reads the ConfigMap (empty leaves the CRD default) | `""` |
| `grafana.dashboards.operator.datasources` | Datasource variable remaps (`inputName`/`datasourceName`) | `[]` |

#### Grafana dashboards

`grafana.dashboards.enabled=true` renders one ConfigMap,
`<release>-llmkube-dashboards`, with a key per file in
`charts/llmkube/dashboards/`. Both delivery mechanisms read that single copy of
the JSON:

- **kube-prometheus-stack sidecar** — the ConfigMap carries
  `grafana_dashboard: "true"`, which the sidecar picks up in any namespace it
  is configured to search. This is the default and needs nothing else.
- **grafana-operator** — `grafana.dashboards.operator.enabled=true` adds a
  `GrafanaDashboard` (`grafana.integreatly.org/v1beta1`) per dashboard whose
  `configMapRef` points at the same ConfigMap key. Set
  `operator.instanceSelector` to match your `Grafana` resource; the CRD
  requires the field and rejects changes to it after creation. Set
  `additionalLabels: {}` to drop the sidecar label if you run no sidecar.

#### Dashboards that only have data sometimes

`sglang-dashboard`, `vllm-dashboard`, and `llamacpp-dashboard` read one
runtime's metrics, so they render blank until something serves that runtime —
and blank looks the same as an idle cluster.

With `operator.enabled` and the default `operator.mode: auto`, the chart hands
those three manifests to the operator instead of applying them, and the
operator publishes each one while an `InferenceService` on its runtime exists
and retires it when the last one goes away. Nothing to configure: deploy a
vLLM service and the vLLM dashboard appears. Set `operator.mode: all` to apply
every dashboard at install time instead.

The chart cannot make this call itself — which runtimes exist is an
`InferenceService` fact that does not exist at `helm install` time, and Helm
leaves nothing running to notice later. The sidecar path is unaffected: the
ConfigMap always carries every dashboard the sidecar is asked to load.

Because the operator applies these, Helm does not own them, and disabling the
feature leaves nothing running to retire them. After
`grafana.dashboards.enabled=false`, `operator.enabled=false`, or
`helm uninstall`, clean them up by hand:

```bash
kubectl delete grafanadashboard -A -l dashboards.llmkube.dev/managed-by=llmkube
```

That label is on operator-published dashboards only, so a hand-authored
`GrafanaDashboard` is never caught by it — and for the same reason the operator
will not adopt, overwrite, or retire one that happens to share a name.

Switching `operator.mode` needs no manual step in either direction. Deferred
manifests carry `meta.helm.sh/release-name` and `-namespace`, so `auto` -> `all`
lets Helm adopt what the operator has already published rather than refusing it
as a foreign object; `all` -> `auto` drops the runtime CRs from Helm's manifest
and the operator republishes whichever runtimes are actually serving.

One residue worth knowing: a dashboard adopted that way keeps its
`dashboards.llmkube.dev/managed-by` label, because Helm does not strip fields it
did not set. It is inert — `mode: all` renders no candidate ConfigMap, so the
reconciler returns before looking at anything — but it does mean the `kubectl
delete` above would also catch a live, Helm-owned dashboard. Run that command
only for the disabled or uninstalled case it is written for; a `mode: all`
release is cleaned up by `helm uninstall` itself.

`amd-gpu-observability` (needs AMD GPUs) and `llmkube-slo` (needs
`pyrra.enabled`) have no such signal and are always published. To drop them, or
to narrow the set on the sidecar path, list what you want:

```yaml
grafana:
  dashboards:
    enabled: true
    only:
      - llmkube-inference
      - llmkube-quota
      - vllm-dashboard
```

Every entry is checked on its own, so any name matching no file fails the
render rather than silently shipping less than you asked for.

`allowCrossNamespaceImport` defaults to `true` so the dashboards reach a
Grafana installed in another namespace, which is the usual layout. Turning it
back off on an existing CR is rejected by the CRD's own validation rule
(`disabling spec.allowCrossNamespaceImport requires a recreate to ensure
desired state`), so an upgrade that flips it to `false` fails until the CRs are
deleted.

`operator.datasources` is a literal `${inputName}` -> `datasourceName` replace
over the dashboard JSON before it reaches Grafana. `datasourceName` must be the
datasource **UID**, not its display name: the placeholder sits inside the
dashboards' `"uid"` fields, so a display name lands verbatim as a uid that
resolves to nothing and every panel silently renders "datasource not found"
(unless the UID happens to equal the name). Most of the shipped dashboards read
their datasource from a `DS_PROMETHEUS` template variable;
`amd-gpu-observability.json` uses one named `datasource`. None of them declares
a dashboard `__inputs` block, so both names are plain template variables and
covering every dashboard takes two entries:

```yaml
grafana:
  dashboards:
    enabled: true
    operator:
      enabled: true
      instanceSelector:
        matchLabels:
          dashboards: grafana
      datasources:
        - inputName: DS_PROMETHEUS
          datasourceName: prometheus-uid # the datasource's UID, not its name
        - inputName: datasource
          datasourceName: prometheus-uid
```

The two dashboards under `config/grafana/` are not part of the chart and stay
hand-imported.

### CRD Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `crds.install` | Install CRDs as part of chart | `true` |
| `crds.keep` | Keep CRDs on uninstall | `true` |

### Pyrra Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `pyrra.enabled` | Enable Pyrra SLO integration | `false` |

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

### Custom Image Registry

For air-gapped or enterprise environments with a private registry:

```bash
helm install llmkube charts/llmkube \
  --namespace llmkube-system \
  --create-namespace \
  --set controllerManager.image.registry=my-registry.company.com \
  --set controllerManager.image.repository=llmkube/llmkube-controller \
  --set controllerManager.initContainer.registry=my-registry.company.com \
  --set controllerManager.initContainer.repository=curlimages/curl
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

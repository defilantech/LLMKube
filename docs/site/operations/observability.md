---
title: Observability
description: Wire LLMKube into Prometheus, Grafana and an OTLP collector. The chart's PodMonitors and ServiceMonitor, the llama.cpp and AMD GPU metric contracts, SLOs through Pyrra, and router-proxy tracing.
---

# Observability

Where LLMKube's signals come from:

- **Operator components.** The controller, the Foreman operator, the router
  proxy and the Metal agent export Prometheus metrics; every name, type and
  label is in the [metrics reference](/docs/reference/metrics).
- **Inference pods.** The serving engine's own `/metrics`. For llama.cpp the
  names are pinned in the [llama.cpp metric contract](#llamacpp-metric-contract).
- **AMD GPUs.** Device metrics from the AMD GPU exporter, pinned in the
  [AMD GPU metric contract](#amd-gpu-metric-contract).
- **SLOs.** `spec.slo` on an InferenceService renders a Pyrra
  `ServiceLevelObjective`; see [SLOs and error budgets](#slos-and-error-budgets).
- **Traces.** Only the ModelRouter proxy creates spans; see [Tracing](#tracing).

## Scraping with the Prometheus Operator

The Helm chart ships the scrape objects, all off by default:

| Value | Creates | Scrapes |
| --- | --- | --- |
| `prometheus.serviceMonitor.enabled` | ServiceMonitor | the controller's metrics endpoint |
| `prometheus.inferencePodMonitor.enabled` | PodMonitor | inference pods' `/metrics` (llama.cpp runs with `--metrics`) |
| `prometheus.routerPodMonitor.enabled` | PodMonitor | router-proxy pods (`llmkube_router_*`) |
| `prometheus.prometheusRule.enabled` | PrometheusRule | the chart's alerts and recording rules |

Each takes `additionalLabels` (set them to match your Prometheus's selector;
the default is `prometheus: kube-prometheus-stack-prometheus`) and `interval`.
The PodMonitors also take `relabelings` and `metricRelabelings`, which run
after the chart's own; rewriting or dropping the labels the chart sets breaks
the alerts and dashboards that select on them. Every value is in the
[Helm values reference](/docs/reference/helm-values).

```yaml
prometheus:
  serviceMonitor:
    enabled: true
  inferencePodMonitor:
    enabled: true
  prometheusRule:
    enabled: true
grafana:
  dashboards:
    enabled: true
```

`grafana.dashboards.enabled` ships the dashboards in
[`charts/llmkube/dashboards/`](https://github.com/defilantech/LLMKube/tree/main/charts/llmkube/dashboards)
as ConfigMaps for the kube-prometheus-stack Grafana sidecar;
[`docs/grafana/README.md`](https://github.com/defilantech/LLMKube/blob/main/docs/grafana/README.md)
lists them and covers importing them by hand.

## llama.cpp metric contract

The pinned metric names for the inference tier (issue #700, slice #1186).
Dashboards and alerts key off these names; changing them is a breaking change
to this contract and must update `charts/llmkube/dashboards/llamacpp-dashboard.json`
in the same PR, which is where every `llamacpp:` query lives.

Backend-agnostic: these come from llama.cpp itself, so they are identical on
CUDA, ROCm and Vulkan servers. The AMD-specific device metrics are a separate
contract; see the [AMD GPU metric contract](#amd-gpu-metric-contract).

### Source: llama.cpp `/metrics`

Requires the server to run with `--metrics`. Scraped by the chart's
`inference-podmonitor.yaml` (`prometheus.inferencePodMonitor.enabled`, off by
default), which selects on the operator's `inference.llmkube.dev/service` label
and relabels every series with `service`, `model`, `runtime` and `namespace`, so
a panel can break down per model without naming a target.

<!-- metrics-contract:begin -->

#### Throughput

| Metric | Type | Unit | Notes |
| --- | --- | --- | --- |
| `llamacpp:predicted_tokens_seconds` | gauge | tokens/s | **decode** throughput (generation) |
| `llamacpp:prompt_tokens_seconds` | gauge | tokens/s | **prefill** throughput (prompt processing) |

Keep these separate rather than presenting one "tokens per second". Prefill is
compute-bound and decode is memory-bandwidth-bound, so they degrade
independently, and a combined figure hides the case where one collapses while
the other holds. Observed in practice: a server whose decode fell from 56 to
5 tokens/s while prefill stayed at its usual rate; a single averaged number
would have read as a mild dip rather than a 10× regression.

#### Saturation

| Metric | Type | Unit | Notes |
| --- | --- | --- | --- |
| `llamacpp:requests_processing` | gauge | count | requests in flight |
| `llamacpp:requests_deferred` | gauge | count | requests queued behind a full slot set: **the queueing signal** |
| `llamacpp:n_busy_slots_per_decode` | gauge | count | average busy slots per `llama_decode()` call |

`llamacpp:requests_deferred` is what distinguishes "the server is busy" from "the server
is oversubscribed": it goes above zero only once every parallel slot is
occupied. Alert on it rather than inferring queueing from latency.

#### Totals

| Metric | Type | Notes |
| --- | --- | --- |
| `llamacpp:n_decode_total` | counter | decode calls |
| `llamacpp:tokens_predicted_total` / `llamacpp:tokens_predicted_seconds_total` | counter | generated tokens and time |
| `llamacpp:prompt_tokens_total` / `llamacpp:prompt_seconds_total` | counter | prompt tokens and time |
| `llamacpp:prompt_tokens_cached_total` | counter | prompt tokens served from cache (prefix-cache effectiveness) |
| `llamacpp:n_tokens_max` | counter | largest observed sequence length (prompt + generation) |
| `llamacpp:spec_decode_num_drafts_total`, `llamacpp:spec_decode_num_draft_tokens_total`, `llamacpp:spec_decode_num_accepted_tokens_total` | counter | speculative decoding; emitted unconditionally, at zero, on a server with no draft model |
| `llamacpp:spec_decode_num_accepted_tokens_per_pos_total` | counter | speculative decoding, labelled by position; the one spec_decode counter that appears only when a draft model is configured |

<!-- metrics-contract:end -->

### Minimum llama.cpp build

Five of the names above are newer than many shipping servers. `prompt_tokens_cached_total`
arrived in `decaf508b` (2026-08-13) and the four `spec_decode_*` counters in
`a035a8887` (2026-08-05), so a server older than mid-August 2026 emits neither
group and a panel keyed on them stays blank rather than erroring. Everything
else in this contract predates that.

### Known gaps (deliberate)

- **KV-cache occupancy is not exposed.** There is no `kv_cache_usage_ratio` or
  equivalent in llama.cpp's `/metrics`. Slice #1186 originally pinned that name
  along with `llamacpp:tokens_per_second`; neither exists. `n_busy_slots_per_decode`
  is a reasonable saturation proxy but measures slot occupancy, not cache
  residency, and should be labelled as such rather than substituted.
- **Per-request attribution**: the metrics are per-server aggregates. Attributing
  throughput to a caller needs the proxy in front of the server, not this endpoint.
- **Context-window pressure**: `n_tokens_max` is a high-water mark, not a
  distribution, so it cannot show how often requests approach the limit.

### Verification

Every metric above was read from a live server rather than from documentation.
Confirmed on a Strix Halo (gfx1151) node and four other InferenceServices, all
scraped through the chart's PodMonitor; types and descriptions are the ones the
server reports in its own metric metadata. The `spec_decode_*` split was checked
against a build `b10795` server started without `--spec-type`, which emits the
first three counters at zero and omits
`llamacpp:spec_decode_num_accepted_tokens_per_pos_total` entirely.

## AMD GPU metric contract

The pinned metric names for the AMD tier (issue #700, slices #1185/#1187).
Dashboards and alerts key off these names; changing them is a breaking change
to this contract and must update `charts/llmkube/dashboards/amd-gpu-observability.json`
in the same PR.

### Source: amdgpu-exporter (DaemonSet, `config/monitoring/amdgpu-exporter.yaml`)

`ghcr.io/defilantech/llmkube-amdgpu-exporter` (this org's `llmkube-runtimes`
repo) reads amdgpu **sysfs/hwmon** directly, with no `rocm-smi`/ROCm userspace
required, which is what makes it work on Vulkan-first boxes (gfx1151) where
the datacenter exporters (`device-metrics-exporter`, `amd_smi_exporter`) do
not enumerate the iGPU.

Every per-GPU series carries `card` (e.g. `card0`) and `pci_slot`
(e.g. `0000:c5:00.0`). **`pci_slot` is the stable join/alert key**; card
indices can move across reboots.

| Metric | Unit | Notes |
| --- | --- | --- |
| `amdgpu_gpus_discovered` | count | AMD DRM devices found (vendor `0x1002`) |
| `amdgpu_gpu_info` | 1 | static labels: device/subsystem/revision ids, product name |
| `amdgpu_gpu_busy_percent` | % | GPU engine busy |
| `amdgpu_memory_busy_percent` | % | memory controller busy (absent on some APUs) |
| `amdgpu_vram_used_bytes` / `amdgpu_vram_total_bytes` | bytes | dedicated VRAM |
| `amdgpu_visible_vram_used_bytes` / `_total_bytes` | bytes | CPU-visible VRAM |
| `amdgpu_gtt_used_bytes` / `amdgpu_gtt_total_bytes` | bytes | **GTT: on APUs the model weights live here**, so this is the key memory signal |
| `amdgpu_temperature_celsius{sensor}` | °C | hwmon temp sensors (`edge`, …) |
| `amdgpu_power_watts{sensor,type}` | W | hwmon power (`type`: `average`/`cap`/…) |
| `amdgpu_clock_hertz{sensor}` | Hz | hwmon freq sensors; **`sensor="sclk"` is the core clock**. `mclk` is not exposed via hwmon on gfx1151 (only `pp_dpm_mclk`); memory clock is a known gap |
| `amdgpu_voltage_volts{sensor}` | V | hwmon voltage |
| `amdgpu_fan_rpm{sensor}` / `amdgpu_fan_pwm{sensor}` | rpm / raw | absent on fanless boxes |
| `amdgpu_pcie_replay_total` | counter | PCIe replay events |
| `amdgpu_scrape_success` / `amdgpu_scrape_failures_total` / `amdgpu_last_scrape_duration_seconds` | | exporter health |

Per-metric failures are isolated: a missing sysfs attribute skips that one
metric, never the scrape. A node with zero AMD GPUs serves an empty-but-healthy
`/metrics` (`amdgpu_gpus_discovered 0`).

### Cross-check: node-exporter hwmon

node-exporter's hwmon collector independently exposes the same sensors as
`node_hwmon_temp_celsius` / `node_hwmon_power_watt`, joinable on
`node_hwmon_chip_names{chip_name="amdgpu"}`. The dashboard's health row uses
these (verified on gfx1151); the `amdgpu_*` equivalents are the per-GPU-labeled
versions of the same readings.

### Inference signals (slice #1186)

<!-- metrics-contract:begin -->
SLO metrics come from llama.cpp `/metrics` (`--metrics`), not this exporter:
`llamacpp:predicted_tokens_seconds`, `llamacpp:prompt_tokens_seconds`,
`llamacpp:requests_processing`, `llamacpp:requests_deferred`. Backend-agnostic
and already emitted. Pinned in the [llama.cpp metric contract](#llamacpp-metric-contract); scraped by the chart's
`inference-podmonitor.yaml`.
<!-- metrics-contract:end -->

### Known gaps (deliberate)

- Memory clock (`mclk`): only available via `pp_dpm_mclk` text parsing; not
  yet exported.
- Per-process attribution: not available from sysfs; would need ROCm userspace.
- Instinct/MI datacenter tiers: use `ROCm/device-metrics-exporter` when that
  hardware exists; this exporter targets the iGPU/APU class it was built on.

## SLOs and error budgets

`spec.slo` lets an `InferenceService` declare a service-level objective directly, without hand-rolling Prometheus recording rules or burn-rate math. When set, the LLMKube operator renders a [Pyrra](https://github.com/pyrra-dev/pyrra) `ServiceLevelObjective` resource in the same namespace; Pyrra (a CNCF-incubating project) turns that declaration into Prometheus recording rules, an error-budget calculation, and multi-window multi-burn-rate alerts.

The division of labor is deliberate: LLMKube owns the ergonomics (one small `spec.slo` block instead of a separate Pyrra CRD referencing your service by hand) and lifecycle (the rendered resource is labeled and owner-referenced so renames, indicator switches, and deletions clean up after themselves). Pyrra owns everything downstream of that: the PromQL, the recording rules, the alerting thresholds, and the error-budget arithmetic. LLMKube does not reimplement any of it.

If you are new to the underlying alerting model, Google's SRE workbook chapter on [alerting on SLOs](https://sre.google/workbook/alerting-on-slos/) is the canonical reference for multi-window multi-burn-rate alerting, the technique Pyrra implements.

### Prerequisites

1. **kube-prometheus-stack** (or equivalent) with the LLMKube PodMonitor enabled:

   ```yaml
   # values.yaml
   prometheus:
     inferencePodMonitor:
       enabled: true
   ```

   Availability SLOs are computed from Prometheus's own `up` series for the scraped pod, so without the PodMonitor there is nothing for Pyrra to read.

2. **Pyrra v0.10.1 or newer**, installed separately. LLMKube's chart does **not** install Pyrra; this integration is tested against v0.10.1's `kubernetes` operator mode:

   ```bash
   # The ServiceLevelObjective CRD
   kubectl apply -f https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/setup/pyrra-slo-CustomResourceDefinition.yaml

   # The kubernetes-mode operator (watches ServiceLevelObjectives, writes PrometheusRules)
   kubectl apply -f https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/pyrra-kubernetesServiceAccount.yaml
   kubectl apply -f https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/pyrra-kubernetesClusterRole.yaml
   kubectl apply -f https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/pyrra-kubernetesClusterRoleBinding.yaml
   kubectl apply -f https://raw.githubusercontent.com/pyrra-dev/pyrra/v0.10.1/examples/kubernetes/manifests/pyrra-kubernetesDeployment.yaml
   ```

   See the [Pyrra repository](https://github.com/pyrra-dev/pyrra) for its full install docs, including the optional API/UI component, which the manifests above do not include.

3. **The chart flag**, off by default:

   ```yaml
   # values.yaml
   pyrra:
     enabled: true
   ```

   This sets the operator's `--enable-pyrra-slo` flag. With the flag off, an `InferenceService` that sets `spec.slo` gets `SLOReady=False` with reason `IntegrationDisabled`; nothing is rendered.

### Availability SLO

Availability is the default indicator and requires no extra configuration beyond an objective:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama
spec:
  modelRef: tinyllama
  slo:
    objective: "99.5"        # 99.5% of scrapes healthy over 28d
```

`indicator` defaults to `availability`, measured as Prometheus scrape success (the `up` series) for the serving pod: every failed scrape counts against the error budget. `objective` must be a number between 50 and 99.999 (a percentage); values outside that range are rejected at admission. `window` defaults to `28d`. `name` defaults to `<inferenceservice-name>-<indicator>` (here, `tinyllama-availability`); set it explicitly if you want a specific name in Pyrra and Grafana.

### Latency SLO

Latency SLOs are **vLLM-only in v0.1**: the indicator needs a request-duration histogram, and llama.cpp does not export one (see [Limitations and roadmap](#limitations-and-roadmap)).

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: vllm-tinyllama
spec:
  modelRef: tinyllama-1b
  runtime: vllm
  slo:
    indicator: latency
    objective: "95"
    latencyThreshold: "2"
    window: 28d
```

Read this honestly: **95% of requests complete within 2 seconds over the window; the percentile lives in the objective, not in a separate p95/p99 field.** There is no way to express "p99 latency under 2s" directly; instead you pick the threshold (`latencyThreshold`) and the fraction of requests that must beat it (`objective`), which is exactly how Pyrra's own latency indicator works.

`latencyThreshold` is a request-duration bound in seconds and must match one of the runtime's histogram bucket boundaries (`le` label), or Pyrra's rate calculation silently returns no data for a boundary that does not exist. For the vLLM runtime, the metric is `vllm:e2e_request_latency_seconds_bucket`, and its buckets (as shipped in LLMKube's default image, `vllm/vllm-openai:v0.20.0`) are, in seconds:

| Bucket boundaries (`le`, seconds) |
| --- |
| 0.3 |
| 0.5 |
| 0.8 |
| 1 |
| 1.5 |
| 2 |
| 2.5 |
| 5 |
| 10 |
| 15 |
| 20 |
| 30 |
| 40 |
| 50 |
| 60 |
| 120 |
| 240 |
| 480 |
| 960 |
| 1920 |
| 7680 |

If your `InferenceService` pins a custom `spec.image` with a different vLLM version, that image's bucket boundaries may differ; check its `/metrics` endpoint for `vllm:e2e_request_latency_seconds_bucket` before choosing a threshold.

Every other runtime, including llama.cpp, has no entry in this table because it exports no request-latency histogram upstream. Setting `indicator: latency` on a non-vLLM `InferenceService` does not fail validation (the CRD cannot know your runtime's metrics), but the SLO reconciler refuses to render anything for it: `SLOReady` goes `False` with reason `IndicatorUnsupportedForRuntime`, and the operator also removes any previously-rendered SLO for that service so a stale one does not keep alerting.

### Reading error budgets

Two ways to see how much error budget is left:

- **Pyrra's own UI** (the `pyrra-api` component), if you installed it alongside the `kubernetes` operator manifests above. It renders each `ServiceLevelObjective` with its current error budget, burn rate, and the underlying PromQL.
- **The `charts/llmkube/dashboards/llmkube-slo.json` reference dashboard**, which reads the same Pyrra-generated recording rules directly from Prometheus: error budget remaining per SLO and burn-rate panels across Pyrra's short and long alerting windows. Import it the same way as the other dashboards in [`docs/grafana/README.md`](https://github.com/defilantech/LLMKube/blob/main/docs/grafana/README.md), or ship it with the chart via `grafana.dashboards.enabled=true`.

Pyrra generates multi-window multi-burn-rate alerts for every `ServiceLevelObjective`: recording rules named `<metric>:burnrate<duration>` at several windows (short windows like 5m/30m paired with longer windows like 1h/6h, per the Google SRE workbook methodology linked above), plus a `PrometheusRule` that fires only when both the short and the matching long window are burning budget fast enough to exhaust it well before the objective's window ends. That pairing is what keeps the alerts both fast (a real incident pages within minutes) and precise (a brief blip that self-resolves does not).

### Conditions reference

The SLO reconciler owns two condition types on `InferenceService.status.conditions`:

#### `SLOReady`

| Reason | Status | Meaning |
| --- | --- | --- |
| `SLOCreated` | True | The Pyrra `ServiceLevelObjective` was rendered and applied. |
| `IntegrationDisabled` | False | `spec.slo` is set, but the chart's `pyrra.enabled` (operator `--enable-pyrra-slo`) is off. |
| `PyrraNotInstalled` | False | `spec.slo` is set and the integration is enabled, but the `pyrra.dev` CRD is not registered in the cluster. |
| `IndicatorUnsupportedForRuntime` | False | The indicator (currently only `latency`) has no metric source on this `InferenceService`'s runtime. |
| `ReconcileFailed` | False | Applying the rendered resource to the API server failed. |

#### `SLODataSourceAvailable`

Only set once `SLOReady` is `True`; it warns when the SLO renders but has nowhere to get data from.

| Reason | Status | Meaning |
| --- | --- | --- |
| `InClusterRuntime` | True | The serving pod is scraped by cluster Prometheus; the SLO has a real data source. |
| `OffClusterRuntime` | False | The `InferenceService` runs on a Metal-backed node, off-cluster. Cluster Prometheus has no scrape target for it, so the rendered SLO will show no data until a metrics path for off-cluster runtimes exists. |

#### Lifecycle behavior worth knowing

- **Unsetting `spec.slo`** deletes the Pyrra resource the operator previously rendered; only the `InferenceService`'s owner reference is not enough to clean this up, because owner references garbage-collect on deletion of the owner, not on a field being cleared.
- **Renaming** the SLO (changing `spec.slo.name` or `spec.slo.indicator`) deletes the stale, previously-rendered resource and creates the new one under the new name.
- **Deleting the `InferenceService`** garbage-collects the rendered Pyrra resource via the owner reference, same as any other owned object.
- **Disabling `pyrra.enabled`** after SLOs were already rendered does not delete them: the reconciler only stops reconciling and reports `IntegrationDisabled` on services that still have `spec.slo` set. The stale Pyrra resources (and their alerts) keep running until the `InferenceService` itself is deleted or `spec.slo` is removed.

### Limitations and roadmap

- **`error_rate` indicator**: the original proposal in [#415](https://github.com/defilantech/LLMKube/issues/415) scoped an `error_rate` indicator alongside `availability` and `latency`. It is deferred: LLMKube has no first-class per-request error metric from serving pods yet, and Pyrra's ratio indicator needs one. A follow-up issue tracking this will be filed against the LLMKube repository.
- **Metal / off-cluster data path**: SLOs render correctly for Metal-backed `InferenceServices` (see `SLODataSourceAvailable=False/OffClusterRuntime` above), but show no data because cluster Prometheus cannot scrape an off-cluster agent. Closing this gap needs a metrics federation path from Metal nodes back to cluster Prometheus, tracked separately.
- **Auto-remediation on SLO breach** (e.g., automatically scaling or rolling back when an error budget burns down) is out of scope for this integration; see [#10](https://github.com/defilantech/LLMKube/issues/10).
- **Custom indicators and multi-cluster SLO aggregation** are not planned for v0.1; see [#415](https://github.com/defilantech/LLMKube/issues/415) for the full scope this integration draws from.

## Tracing

What LLMKube traces today, and how to send those spans to Tempo or any other
OTLP collector.

### What is traced

Only the ModelRouter proxy (`router-proxy`) creates spans. The controller
manager installs a TracerProvider from the same settings, but it creates no
spans of its own, so it exports nothing. LLMKube does not instrument the
inference pods (llama.cpp, vLLM and the other runtimes) or the metal-agent.

The proxy traces each request to `/v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/rerank`, `/v1/messages` and
`/v1/messages/count_tokens` with two spans, exported under the service name
`llmkube-router-proxy`:

| Span | Covers | Attributes |
| --- | --- | --- |
| `model_router.dispatch` | The request from the routing decision to the last byte streamed to the client. | `routing.classification`; `routing.rule.matched` and `routing.strategy` when a rule matched; `routing.backend.selected` and `routing.backend.tier` once a backend answered. |
| `backend.request` | One attempt at one backend, as a child of the dispatch span, until response headers arrive or the attempt fails. A fallback adds one span per backend tried. | `routing.backend.selected`, `routing.backend.provider`, `routing.backend.tier`, `routing.fallback.depth` (0 for the first backend). |

A ModelPool swap wait falls inside `model_router.dispatch`, before its
`backend.request` child starts, so it shows as the gap ahead of that child.

Not traced:

- Requests the proxy answers before dispatching: a body it cannot read (400),
  no matching route (503), a fail-closed refusal (503) and an exhausted budget
  (429). The audit log records the last three.
- The copies a shadow rule sends to its other backends.
- Relay pods (the router-proxy binary in metal-agent relay mode).

Two more limits:

- Spans carry no error status. A failed attempt ends its `backend.request`
  span like a successful one; read the outcome from the audit log or the
  router metrics.
- Trace context is not propagated. The proxy neither continues an incoming
  `traceparent` header nor sends one upstream, so each request is the root of
  its own trace.

### Turning it on

Set the standard OpenTelemetry variables on the controller manager. With the
Helm chart:

```yaml
controllerManager:
  env:
    - name: OTEL_EXPORTER_OTLP_ENDPOINT
      value: http://tempo.monitoring.svc.cluster.local:4317
```

The controller copies its exporter settings into every router-proxy
Deployment it renders, so the proxy pods roll and start exporting. There is no
per-ModelRouter setting, and env added to a proxy Deployment by hand does not
last: the controller owns that pod spec and rewrites it on the next reconcile.

The proxy logs `OpenTelemetry tracing enabled` with the endpoint at startup.
If it logs `OpenTelemetry tracing disabled` instead, the error says why. Spans
are sent in batches and flushed when the proxy shuts down gracefully, inside
its drain budget.

#### Variables

| Variable | Effect | Copied to proxy pods |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Collector address; the traces-specific one wins. Setting either turns export on. | Yes |
| `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_EXPORTER_OTLP_TRACES_INSECURE` | `true` sends plaintext to an endpoint given without a scheme. | Yes |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Sampling. The default traces every request; on a busy router consider `parentbased_traceidratio` with a ratio. | Yes |
| `OTEL_SDK_DISABLED=true`, `OTEL_TRACES_EXPORTER=none` | Turn export off. | No: with the controller's export off, it copies nothing |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Resource attributes. Defaults: `llmkube-controller` for the controller, `llmkube-router-proxy` for the proxy. | No: they would name the proxy after the controller |
| `OTEL_EXPORTER_OTLP_HEADERS` | Request headers, for example a collector token. | No: the value would land as a plain env var on a Deployment in every namespace that has a ModelRouter |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_KEY` | TLS files. | No: the paths exist only in the controller pod |

So a collector that needs auth headers or a private CA works for the
controller but not for the proxies today.

#### Endpoint format

- `http://host:4317` is plaintext gRPC and `https://host:4317` is TLS.
- `host:4317` with a hostname is TLS unless `OTEL_EXPORTER_OTLP_INSECURE=true`.
  Earlier releases forced plaintext for the controller; if you set a bare
  `host:4317` before, add `http://` or set the insecure variable.
- A bare IP and port such as `10.0.0.5:4317` is refused with an error in the
  log, and tracing stays off. The OpenTelemetry Go exporter cannot parse it
  and would otherwise send to `localhost:4317`. Write `http://10.0.0.5:4317`.
- The exporter speaks OTLP over gRPC only. An OTLP/HTTP endpoint (port 4318)
  does not work, and `OTEL_EXPORTER_OTLP_PROTOCOL` is ignored.
- Use a fully qualified Service name. The proxy pods run in each ModelRouter's
  namespace, so a short name such as `tempo:4317` that resolves from the
  controller's namespace does not resolve from theirs.
- LLMKube's router NetworkPolicy restricts ingress only. If your cluster
  restricts egress from ModelRouter namespaces, allow the proxy pods to reach
  the collector port.

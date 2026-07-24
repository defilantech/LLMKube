# SLOs and error budgets

`spec.slo` lets an `InferenceService` declare a service-level objective directly, without hand-rolling Prometheus recording rules or burn-rate math. When set, the LLMKube operator renders a [Pyrra](https://github.com/pyrra-dev/pyrra) `ServiceLevelObjective` resource in the same namespace; Pyrra (a CNCF-incubating project) turns that declaration into Prometheus recording rules, an error-budget calculation, and multi-window multi-burn-rate alerts.

The division of labor is deliberate: LLMKube owns the ergonomics (one small `spec.slo` block instead of a separate Pyrra CRD referencing your service by hand) and lifecycle (the rendered resource is labeled and owner-referenced so renames, indicator switches, and deletions clean up after themselves). Pyrra owns everything downstream of that: the PromQL, the recording rules, the alerting thresholds, and the error-budget arithmetic. LLMKube does not reimplement any of it.

If you are new to the underlying alerting model, Google's SRE workbook chapter on [alerting on SLOs](https://sre.google/workbook/alerting-on-slos/) is the canonical reference for multi-window multi-burn-rate alerting, the technique Pyrra implements.

## Prerequisites

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

## Availability SLO

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

## Latency SLO

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

## Reading error budgets

Two ways to see how much error budget is left:

- **Pyrra's own UI** (the `pyrra-api` component), if you installed it alongside the `kubernetes` operator manifests above. It renders each `ServiceLevelObjective` with its current error budget, burn rate, and the underlying PromQL.
- **The `charts/llmkube/dashboards/llmkube-slo.json` reference dashboard**, which reads the same Pyrra-generated recording rules directly from Prometheus: error budget remaining per SLO and burn-rate panels across Pyrra's short and long alerting windows. Import it the same way as the other dashboards in `docs/grafana/README.md`, or ship it with the chart via `grafana.dashboards.enabled=true`.

Pyrra generates multi-window multi-burn-rate alerts for every `ServiceLevelObjective`: recording rules named `<metric>:burnrate<duration>` at several windows (short windows like 5m/30m paired with longer windows like 1h/6h, per the Google SRE workbook methodology linked above), plus a `PrometheusRule` that fires only when both the short and the matching long window are burning budget fast enough to exhaust it well before the objective's window ends. That pairing is what keeps the alerts both fast (a real incident pages within minutes) and precise (a brief blip that self-resolves does not).

## Conditions reference

The SLO reconciler owns two condition types on `InferenceService.status.conditions`:

### `SLOReady`

| Reason | Status | Meaning |
| --- | --- | --- |
| `SLOCreated` | True | The Pyrra `ServiceLevelObjective` was rendered and applied. |
| `IntegrationDisabled` | False | `spec.slo` is set, but the chart's `pyrra.enabled` (operator `--enable-pyrra-slo`) is off. |
| `PyrraNotInstalled` | False | `spec.slo` is set and the integration is enabled, but the `pyrra.dev` CRD is not registered in the cluster. |
| `IndicatorUnsupportedForRuntime` | False | The indicator (currently only `latency`) has no metric source on this `InferenceService`'s runtime. |
| `ReconcileFailed` | False | Applying the rendered resource to the API server failed. |

### `SLODataSourceAvailable`

Only set once `SLOReady` is `True`; it warns when the SLO renders but has nowhere to get data from.

| Reason | Status | Meaning |
| --- | --- | --- |
| `InClusterRuntime` | True | The serving pod is scraped by cluster Prometheus; the SLO has a real data source. |
| `OffClusterRuntime` | False | The `InferenceService` runs on a Metal-backed node, off-cluster. Cluster Prometheus has no scrape target for it, so the rendered SLO will show no data until a metrics path for off-cluster runtimes exists. |

### Lifecycle behavior worth knowing

- **Unsetting `spec.slo`** deletes the Pyrra resource the operator previously rendered; only the `InferenceService`'s owner reference is not enough to clean this up, because owner references garbage-collect on deletion of the owner, not on a field being cleared.
- **Renaming** the SLO (changing `spec.slo.name` or `spec.slo.indicator`) deletes the stale, previously-rendered resource and creates the new one under the new name.
- **Deleting the `InferenceService`** garbage-collects the rendered Pyrra resource via the owner reference, same as any other owned object.
- **Disabling `pyrra.enabled`** after SLOs were already rendered does not delete them: the reconciler only stops reconciling and reports `IntegrationDisabled` on services that still have `spec.slo` set. The stale Pyrra resources (and their alerts) keep running until the `InferenceService` itself is deleted or `spec.slo` is removed.

## Limitations and roadmap

- **`error_rate` indicator**: the original proposal in [#415](https://github.com/defilantech/LLMKube/issues/415) scoped an `error_rate` indicator alongside `availability` and `latency`. It is deferred: LLMKube has no first-class per-request error metric from serving pods yet, and Pyrra's ratio indicator needs one. A follow-up issue tracking this will be filed against the LLMKube repository.
- **Metal / off-cluster data path**: SLOs render correctly for Metal-backed `InferenceServices` (see `SLODataSourceAvailable=False/OffClusterRuntime` above), but show no data because cluster Prometheus cannot scrape an off-cluster agent. Closing this gap needs a metrics federation path from Metal nodes back to cluster Prometheus, tracked separately.
- **Auto-remediation on SLO breach** (e.g., automatically scaling or rolling back when an error budget burns down) is out of scope for this integration; see [#10](https://github.com/defilantech/LLMKube/issues/10).
- **Custom indicators and multi-cluster SLO aggregation** are not planned for v0.1; see [#415](https://github.com/defilantech/LLMKube/issues/415) for the full scope this integration draws from.

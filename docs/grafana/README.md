# LLMKube Grafana dashboards

Starter dashboards for visualizing LLMKube inference workloads. They consume
the recording rules + metric labels installed by the LLMKube Helm chart's
PrometheusRule and PodMonitor templates.

## Prerequisites

- `prometheus.inferencePodMonitor.enabled=true` in your Helm values
- `prometheus.prometheusRule.enabled=true` in your Helm values
- A Prometheus datasource named `Prometheus` (or use the `DS_PROMETHEUS`
  template variable to pick a different one at import time)
- Grafana 10 or newer
- For `llmkube-slo.json` only: the operator running with `--enable-pyrra-slo`
  and the Pyrra kubernetes operator installed, so `spec.slo` InferenceServices
  have a PrometheusRule generated for their ServiceLevelObjective. See
  `docs/observability/slo.md`.

## Files

| File | Description |
|---|---|
| `llmkube-inference.json` | Request latency, TTFT (vLLM only), GPU queue wait, container restart rate. Grouped by service, runtime, namespace. |
| `llmkube-slo.json` | Error budget remaining and multi-window burn rate for InferenceServices with `spec.slo` set, plus an SLO overview table. Reads the recording rules Pyrra's kubernetes operator writes per SLO. Templated on a `$slo` variable (`label_values(slo)`) and a manual `$objective` percentage variable, since Pyrra does not expose the target itself as a Prometheus series. Assumes the default 28d SLO window; see the dashboard description for details. |

## Importing

In Grafana: `+ Import` -> upload JSON -> select datasource -> Import.

Or via API:

```bash
curl -sX POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GRAFANA_TOKEN" \
  -d @llmkube-inference.json \
  https://grafana.example.com/api/dashboards/db
```

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

## Files

| File | Description |
|---|---|
| `llmkube-inference.json` | Request latency, TTFT (vLLM only), GPU queue wait, container restart rate. Grouped by service, runtime, namespace. |

## Importing

In Grafana: `+ Import` -> upload JSON -> select datasource -> Import.

Or via API:

```bash
curl -sX POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GRAFANA_TOKEN" \
  -d @llmkube-inference.json \
  https://grafana.example.com/api/dashboards/db
```

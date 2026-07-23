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
| `llmkube-inference.json` | Request latency (vLLM only) and container restart rate. Latency is grouped by service, runtime, namespace; restarts by pod, container, namespace. |
| `llmkube-slo.json` | Error budget remaining and multi-window burn rate for InferenceServices with `spec.slo` set, plus an SLO overview table. Reads the recording rules Pyrra's kubernetes operator writes per SLO. Templated on a `$slo` variable (`label_values(slo)`) and a manual `$objective` percentage variable, since Pyrra does not expose the target itself as a Prometheus series. Assumes the default 28d SLO window; see the dashboard description for details. |
| `amd-gpu-observability.json` | AMD GPU health, memory, and inference SLO signals for Strix (gfx1151) nodes. Reads the amdgpu-sysfs exporter, node-exporter hwmon, and the `llamacpp:*` series llama.cpp exposes on `/metrics`. Panels cover GPU temperature, power, busy %, GTT/VRAM memory, GPU clock, tokens/sec, and in-flight requests. |
| `llmkube-quota.json` | Per-GPUQuota usage against the declared GPU and VRAM caps, plus admission denials. |
| `model-router-dashboard.json` | Router-proxy request rate, latency, backend health and fail-closed rejections. **Renders empty today:** `cmd/router-proxy` mounts no `/metrics` handler, so the `llmkube_router_*` collectors it registers are never scraped. The panels are correct and will populate once the endpoint is served. |

## Importing

In Grafana: `+ Import` -> upload JSON -> select datasource -> Import.

Or via API:

```bash
curl -sX POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $GRAFANA_TOKEN" \
  -d @llmkube-inference.json \
  https://grafana.example.com/api/dashboards/db
```

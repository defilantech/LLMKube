# Tracing

What LLMKube traces today, and how to send those spans to Tempo or any other
OTLP collector.

## What is traced

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

## Turning it on

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

### Variables

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

### Endpoint format

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

---
title: Model Router
description: A single OpenAI-compatible endpoint that routes across local InferenceServices and external providers (Anthropic, OpenAI, LiteLLM) with policy-driven handoff, fail-closed semantics for regulated data, and audit log per request.
updated: 2026-10-16
---

# Model Router

The `ModelRouter` CRD exposes a single OpenAI-compatible HTTP endpoint that dispatches requests across multiple backends:

- Local `InferenceService` instances managed by LLMKube
- External providers (Anthropic, OpenAI) called directly
- A LiteLLM proxy that aggregates many providers behind one URL

It is the **cross-engine handoff layer** for agentic chains: an agent running against a local model can transparently call out to a cloud model for specific steps, governed by declarative policy that enforces data classification, cost, and capability constraints.

<DocCallout variant="note" title="Status">

ModelRouter is **alpha** as of LLMKube 0.8. The CRD lives in `inference.llmkube.dev/v1alpha1`. The wire shape may evolve before v1beta1.

</DocCallout>

## Why this exists

LLMs are increasingly composed: an agent does some work on a fast local model, hands off a hard step to a frontier cloud model, then comes back. Every team building this hits the same three problems:

1. **The agent code wants one endpoint**, not a dispatch tree. Agent runtimes (LangGraph, CrewAI, OpenAI Agents SDK, Anthropic Agent SDK) all consume a single OpenAI-compatible URL.
2. **Routing policy belongs in the platform**, not the agent. Compliance teams need to know that PII can't egress; finance teams need cost caps; SRE teams need fallback on local outage. None of that should live in Python at the agent level.
3. **The choice between local and cloud changes**. Today's local model handles 80% of requests; tomorrow's handles 95%. The agent shouldn't have to change.

`ModelRouter` solves all three by sitting in the data path as a small managed HTTP proxy with declarative routing rules.

## Architecture

```
   +-------------+
   | Agent / App |
   +------+------+
          |  OpenAI-compatible API
          v
   +----------------------------------------------------+
   |  router-proxy Deployment (controller-managed)      |
   |  - reads compiled config from a mounted ConfigMap  |
   |  - matches each request against ordered rules      |
   |  - enforces the fail-closed gate                   |
   |  - streams responses (SSE / chunked) with no buffer|
   |  - emits one audit-log line per request            |
   +-----+----------------------+-----------------------+
         |                      |
         v                      v
   +-----------+         +-------------------------+
   | local     |         | external provider       |
   | Inference |         | (Anthropic / OpenAI /   |
   | Service   |         |  LiteLLM passthrough)   |
   +-----------+         +-------------------------+
```

The controller compiles `ModelRouter.spec` into a JSON config, writes it to a `ConfigMap`, and reconciles a `Deployment` plus `Service` that mounts the ConfigMap and runs the [router-proxy binary](https://github.com/defilantech/LLMKube/blob/main/cmd/router-proxy). The ConfigMap content is hashed and the hash lands on the pod template annotation, so any spec change triggers a clean rollout.

Since every spec change re-rolls the pods, set `spec.proxy.revisionHistoryLimit` to cap how many old `ReplicaSet`s are kept (`0` keeps none); unset keeps 3. `InferenceService` has the same knob at `spec.revisionHistoryLimit`.

### Draining in-flight streams on a rollout

A proxy replica keeps serving the requests it already holds after `SIGTERM`, for its `--shutdown-timeout`: 30s by default. A streamed completion can run far longer, so a roll cuts any stream still open at that deadline. `spec.proxy.drainTimeout` raises it:

```yaml
spec:
  proxy:
    replicas: 2
    drainTimeout: 600s
```

It sets the proxy's `--shutdown-timeout` and the pod's `terminationGracePeriodSeconds`, which is the drain plus a fixed 5s margin so the kubelet does not SIGKILL before the drain ends. The margin covers process exit only: this pod has no `preStop` hook, and the proxy bounds its own shutdown by the drain. Values run from `1s` to `1h`; a fractional value rounds the grace period up to the next whole second.

A replica that drains stays `Terminating` until its drain ends, so a roll waits up to `drainTimeout` per replica before that pod goes. `replicas: 2` keeps new requests flowing on the other replica while one drains, but it does nothing for the requests already streaming on the replica that rolls, which is what this field bounds. A stream longer than `drainTimeout` still ends at the deadline.

Unset keeps the 30s default and renders nothing on the pod template, so upgrading the operator does not roll existing proxies. Setting or changing it does roll them once.

### Exposing the proxy and sandboxing its pod

These `spec.endpoint` and `spec.proxy` options apply to `dataPlane: Proxy`, the default.

`spec.endpoint` shapes the data-plane `Service` in front of the proxy. `spec.endpoint.port` sets the `Service` port (default `8080`); the proxy itself always listens on `8080`, and the `Service` forwards to it. So `port: 80` gives clients `http://<router>-router-proxy.<namespace>.svc.cluster.local/v1` with no port in the URL, while the proxy still runs unprivileged. For a shorter name today, create your own `Service` that selects the proxy pods (labels `app: <router>-router-proxy` and `inference.llmkube.dev/model-router: <router>`) with `targetPort: 8080`; a configurable name is tracked in #2279. `type: NodePort` or `LoadBalancer` exposes it beyond the cluster. `spec.endpoint.nodePort` pins the node port and applies only to `type: NodePort`; it is ignored for `LoadBalancer`, as on `InferenceService`. When `nodePort` is unset, the port Kubernetes allocated stays the same across reconciles. Removing a previously pinned `nodePort` also keeps that port; to get a new one, change `type` or recreate the `Service`.

`spec.endpoint.externalTrafficPolicy` (`Cluster` or `Local`, only with `type` NodePort or LoadBalancer) is applied to that `Service`. `Local` preserves the client source IP, but traffic only reaches nodes that run a proxy replica, so pair it with `spec.proxy.nodeSelector`, or with one replica per node (below). `spec.endpoint.apiKeySecretRef` is not supported on a `ModelRouter`.

`spec.proxy.runtimeClassName` runs the proxy pod under a RuntimeClass, for example a sandboxed runtime such as gVisor or Kata. Unset leaves the pod spec unchanged.

```yaml
spec:
  endpoint:
    type: NodePort
    nodePort: 30801
    externalTrafficPolicy: Local
  proxy:
    runtimeClassName: gvisor
    nodeSelector:
      kubernetes.io/hostname: edge-1
```

### Labels and annotations on the proxy pods

`spec.proxy.podLabels` and `spec.proxy.podAnnotations` are merged into the proxy pod template, as `spec.podLabels` and `spec.podAnnotations` are on an `InferenceService`. Use them to match label-based NetworkPolicies. A proxy that needs the API server (for ModelPool activation or `resolution: endpoint`) may need such a policy to reach it.

```yaml
spec:
  proxy:
    podLabels:
      netpol.example.com/apiserver: allow
    podAnnotations:
      example.com/team: llm
```

The operator's own keys win on collision: the `app` and `inference.llmkube.dev/model-router` labels, the config-hash annotation, and the `prometheus.io` annotations when the operator emits them. The `Deployment` and `Service` selectors use only the operator's labels, so changing these fields rolls the pods but never changes a selector. A key you remove from the spec is removed from the pods. The operator records the keys it applied in the `inference.llmkube.dev/applied-pod-labels` and `inference.llmkube.dev/applied-pod-annotations` annotations on the `Deployment`. Keys other tools put on the pod template, such as `kubectl rollout restart`'s, are left alone.

### One proxy replica per node

`spec.proxy.affinity` and `spec.proxy.topologySpreadConstraints` are passed through to the proxy pod, as on `InferenceService`. The proxy pods carry two stable labels to select on: `app: <router>-router-proxy` and `inference.llmkube.dev/model-router: <router>`.

For a highly available edge on two GPU nodes, run one replica per node and send each node's traffic to its own replica with `externalTrafficPolicy: Local`. An upstream that hashes the session header across the nodes' addresses then keeps every session on one replica, and its session bindings stay on that replica:

```yaml
metadata:
  name: all-models
spec:
  endpoint:
    type: NodePort
    nodePort: 30802
    externalTrafficPolicy: Local
  proxy:
    replicas: 2
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app: all-models-router-proxy
```

With `replicas` above 1 and a hard placement rule (a constraint with `whenUnsatisfiable: DoNotSchedule`, or required pod anti-affinity), the proxy Deployment rolls out with `maxSurge: 0` and `maxUnavailable: 1`: an extra replica could never be placed, so the default surge would hang the rollout. The proxy rolls on every LLMKube upgrade, and while one replica restarts, its node's NodePort has no local endpoint, so the upstream should fail over to the other node's address. Soft rules (`ScheduleAnyway`, preferred affinity) and single-replica routers keep the default rollout. A router with ModelPools always runs one replica.

## The fail-closed gate

The headline differentiator. A rule that matches sensitive classifications (default: `pii`, `phi`) and is marked `failClosed: true` has two guarantees:

1. **Cannot reference cloud-tier backends.** The controller rejects the manifest at `kubectl apply` if it tries to. This is the static half of the gate.
2. **Refuses rather than falls through.** At request time, if every backend in the route is unhealthy, the proxy returns HTTP 503 and emits an audit-log denial. It does **not** fall through to other rules or to `defaultRoute`. Sensitive data never leaves the cluster, even on outage.

This is the property regulated industries (healthcare, finance, defense, manufacturing) need to adopt local LLM inference at all.

## Minimal example

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: coding-router
spec:
  backends:
    - name: local-qwen
      tier: local
      inferenceServiceRef:
        name: qwen3-coder
    - name: cloud-opus
      tier: cloud
      external:
        provider: anthropic
        model: claude-opus-4-7
        url: https://api.anthropic.com
        credentialsSecretRef:
          name: anthropic-key

  rules:
    - name: pii-stays-local
      match:
        dataClassification: ["pii", "phi"]
      route:
        backends: ["local-qwen"]
      failClosed: true

    - name: complex-to-cloud
      match:
        taskComplexity: complex
      route:
        backends: ["cloud-opus", "local-qwen"]
        strategy: primary-fallback

  defaultRoute: local-qwen
```

After `kubectl apply`:

- `kubectl describe modelrouter coding-router` shows the status conditions and per-backend health.
- `kubectl get configmap coding-router-router-proxy` contains the compiled JSON config.
- The endpoint is `http://coding-router-router-proxy.<namespace>.svc.cluster.local:8080/v1/chat/completions`.

Point any OpenAI-compatible client at that URL and it just works. The proxy serves the OpenAI-compatible routes, and the Anthropic Messages API, at their standard paths, and every POST route is matched on the request's `model` field against the same rule set:

| Route | Serves |
|---|---|
| `POST /v1/chat/completions` | Chat completions |
| `POST /v1/completions` | Legacy completions: what fill-in-the-middle / IDE autocomplete clients and benchmark tools such as `vllm bench serve --backend openai` send |
| `POST /v1/embeddings` | Embeddings |
| `POST /v1/rerank` | Reranking |
| `POST /v1/messages` | Anthropic Messages API: Claude Code and the Anthropic SDKs |
| `POST /v1/messages/count_tokens` | Anthropic token counting |
| `GET /v1/models` | Model discovery: one entry per backend, under its `displayName`, else its `name`, the id `BackendNameMatch` resolves. An external backend's upstream `model` is not listed. |

The two Anthropic routes are placed, balanced and session-bound exactly like chat completions, and headers such as `anthropic-version` and `anthropic-beta` pass through. The backend must serve them itself: vLLM does natively; a backend that does not returns its own `404`, which reaches the client unchanged and does not count against the backend. Token budgets charge Anthropic responses from their own usage fields (cached input included), and errors the router itself returns on these routes use the Anthropic error shape.

`GET /v1/models` lists each backend under its `displayName`, else its `name`. Coding clients, and services that generate client config, read this list to decide whether a model takes images and how much context it holds. A backend publishes both when it declares `modelInfo`:

```yaml
  backends:
    - name: glm-5-3-flash
      inferenceServiceRef: {name: glm-5-3-flash}
      capabilities: [vision]
      modelInfo: {}
```

That backend's entry then carries `"context_length"` and an OpenRouter-style `architecture` block, `{"modality": "text+image->text", "input_modalities": ["text", "image"], "output_modalities": ["text"]}`. Fields you leave unset are filled in:

| Field | When unset |
|---|---|
| `inputModalities` | `[text, image]` if the backend's `capabilities` include `vision`, else `[text]` |
| `outputModalities` | `[text]` |
| `contextLength` | vLLM: `vllmConfig.maxModelLen`, or `--max-model-len` in `extraArgs`. llama.cpp: `contextSize`, divided across `parallelSlots` when they are set, because llama-server then splits the context between slots. An unset `runtime` counts as llama.cpp except on a Metal Model, where the metal-agent picks the runtime. Other runtimes, and llama.cpp `extraArgs` that set `--ctx-size`, `--parallel` or `--kv-unified`, publish no context length. |

A value you set wins. If `inputModalities` leaves out `image` while `capabilities` lists `vision`, the controller records a `ModelInfoMismatch` warning event, and `/v1/models` publishes `inputModalities`. A backend without `modelInfo` publishes the bare entry, as before.

The router doesn't enforce `modelInfo`, so keep it in step with what the backend serves: a vLLM backend started with `--limit-mm-per-prompt '{"image":0}'` rejects images whatever the list says. `modelInfo` applies to the Proxy data plane only. The Gateway and AgentGateway data planes ignore it, and AgentGateway answers `/v1/models` itself.

Headers that change routing:

| Header | Effect |
|---|---|
| `x-llmkube-classification: pii` | Matches `pii-stays-local` rule; local-only, fail-closed. |
| `x-llmkube-task-complexity: complex` | Matches `complex-to-cloud` rule; tries Opus first, falls back to Qwen on 5xx. |
| (no headers) | Falls through to `defaultRoute: local-qwen`. |

## Rule strategies

`route.strategy` decides how a matched rule uses the backends it lists:

- **`primary-fallback`** (default) tries `route.backends` in order. The first backend serves; the next one is tried only when it fails.
- **`weighted`** splits the rule's traffic across its backends in proportion to each backend's `weight` (default `1`). Use it to keep two kinds of hardware warm behind one model name.
- **`shadow`** is not implemented by the router-proxy yet: a `shadow` rule dispatches like `primary-fallback`. `dataPlane: Gateway` and `dataPlane: AgentGateway` reject it.

Whatever the strategy, a rule with `match.requiredCapabilities` sends its requests only to the backends that advertise every listed capability. The rule matches when at least one of its backends does; the others are never tried for it, not even as a fallback. A rule without `requiredCapabilities` uses all of its backends.

```yaml
spec:
  backends:
    - name: mac-m5
      inferenceServiceRef:
        name: qwen38-metal
      weight: 1
    - name: nvidia-gb10
      inferenceServiceRef:
        name: qwen38-cuda
      weight: 3
  rules:
    - name: qwen38
      match:
        models: ["qwen38-27b"]
      route:
        strategy: weighted
        backends: ["mac-m5", "nvidia-gb10"]
```

On the Proxy data plane, a weighted rule picks a backend for each request at random, weighted by `weight`, among the backends that can serve it right now. A backend that is quarantined after failures, that lacks a capability the rule's `requiredCapabilities` lists, or (under `poolActivation: IfIdle`) whose ModelPool incumbent is busy is skipped, and the pick falls on the others in proportion to their weights. If the picked backend fails the way a primary fails over under `primary-fallback` (a 5xx, a connection error, a header timeout, a ModelPool activation that does not complete), the request moves to another backend of the rule, picked the same way, and never back to one it already tried. A weighted rule is therefore as available as a `primary-fallback` rule over the same backends. The audit line's `backend` field and the `llmkube_router_requests_total` `backend` label show which backend served each request, and budgets charge the backend that served.

`weight: 0` drains a backend from weighted rules: it gets none of their traffic, even while every other backend in the rule is down. This is the Gateway API meaning of a zero weight, which `dataPlane: Gateway` also follows (Envoy Gateway drops a zero-weight `backendRef`), so the same spec splits the same way on both data planes. To keep a backend as a standby for when the others fail, list it in a `primary-fallback` rule instead. A weighted rule needs at least one backend with a positive weight that has every required capability; the controller rejects one that has none.

A weighted rule on the Proxy data plane cannot list two different members of one ModelPool. The members share one GPU slot, so splitting traffic between them would drain one and cold-load the other on a large share of requests. The controller reports this as `Available=False` with reason `CompileFailed` and keeps serving the previous config. Splitting across a pooled backend and a backend outside the pool is fine: picking the pooled one activates it as `primary-fallback` would. `dataPlane: AgentGateway` compiles only `primary-fallback`.

## Default-route strategy

`defaultRouteStrategy` controls what happens to a request that matches no rule, before the `defaultRoute` fallback applies:

- **`Static`** (default) routes every unmatched request to `defaultRoute`.
- **`BackendNameMatch`** first checks whether the request's `model` field exactly equals a backend's `displayName`, else its `name`; if so, it routes straight to that backend. Only when no backend matches does it fall back to `defaultRoute`. This lets clients address a backend directly (`{"model": "cloud-opus"}`) without writing a pass-through rule for each one.

The lookup is against backend `name` — the identifier you publish to clients — never the upstream provider `model`, so the proxied API surface stays on its own side of the boundary. Explicit rules always win: name matching only fires when no rule accepted the request, and it never bypasses a `failClosed` rule.

`BackendNameMatch` widens the trust boundary deliberately: every backend, including cloud-tier ones, becomes directly addressable by any client that names it. That is the point — it lets a single flag replace a per-backend passthrough rule for each model you want clients to reach by name. It does not weaken the fail-closed gate, because `failClosed` rules are ordinary rules and are evaluated first: a sensitive request that matches such a rule never reaches the name-match step. The same caveat as `defaultRoute` applies, though — a sensitive request that matches *no* rule will fall through to name matching (and then `defaultRoute`) exactly as it would fall through to `defaultRoute` under `Static`. Per-rule fail-closed protection is not a global PII-egress invariant; if you route sensitive classifications, gate them with an explicit `failClosed` rule.

A request whose model names a backend that is currently unhealthy fails fast (HTTP 502/503) rather than silently rerouting to `defaultRoute` — naming a backend is a request for *that* backend, and answering from a different model the client did not ask for would be worse than an honest error. This matches how an explicit single-backend rule behaves on backend outage.

## Per-request balancing across replicas

A local backend resolves to its `InferenceService`'s ClusterIP `Service` by default. With several replicas that has a sharp edge: a keep-alive client pins to one pod through the `Service`'s conntrack entry, so one hot client saturates a single replica while the others sit idle. Embeddings and rerank workloads feel this first — they are typically called in tight loops over one connection.

`spec.backends[].resolution: endpoint` changes how the upstream address is compiled. Instead of the `Service` DNS name, the router-proxy addresses **each ready pod** directly (`http://<pod-ip>:<port>`, with the port taken from the referenced `InferenceService`'s `spec.endpoint.port`, default `8080`), tracked out of the `EndpointSlices` behind the backend's `Service`. The router-proxy then rotates each request across those addresses, skips pods quarantined by the per-pod circuit breaker, and considers the backend healthy as long as at least one pod is dispatchable. A pod is quarantined after two failures in a row (a 5xx or a network error), or at once when a stream goes silent past `streamIdleTimeout`; each quarantine is logged at Warn. A request that fails on a pooled keep-alive connection the pod had already closed, before any response byte, is retried once on a fresh connection to the same pod and does not count as a failure (`llmkube_router_dispatch_retries_total{reason="stale_connection"}`). The proxy closes idle upstream connections after 4 seconds, before vLLM's 5-second keep-alive timeout. `service` remains the default, and external backends ignore the field.

The router-proxy tracks those pods itself: it watches the `EndpointSlices` behind each endpoint-resolution backend and updates its list as pods come and go, so a rollout or a scale change never restarts the proxy. The proxy runs under its own ServiceAccount with read access to `EndpointSlices` in the router's namespace. When the controller sees no ready pods (a rollout in progress, or a backend with no `EndpointSlices` at all, such as a metal-agent member), the backend's status reports `no ready endpoints; dispatch falls back to the Service DNS name`. Whether the proxy's own watch has synced is a separate signal, shown only by `llmkube_router_endpoint_source_ready`, which is 0 until the watch syncs (for example when the proxy lacks RBAC to read `EndpointSlices`). Until then dispatch uses the `Service` address.

To run the proxy next to tainted GPU nodes, set `spec.proxy.nodeSelector` and `spec.proxy.tolerations`.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: bge-m3
spec:
  modelRef: bge-m3
  mode: embedding
  replicas: 3
  endpoint:
    port: 8080
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: embedding-router
spec:
  backends:
    - name: bge-embed
      inferenceServiceRef:
        name: bge-m3
      resolution: endpoint
  defaultRoute: bge-embed
```

Use it on multi-replica local backends where one client should not pin one pod. A single replica gains nothing — there is only one endpoint to rotate to — and out-of-cluster backends (the metal-agent path, `external:` providers) have no `EndpointSlices` to compile, so they stay service-level.

## Load-aware balancing

Round-robin spreads requests evenly, but agent sessions carry very different amounts of work: one copy can draw the long, uncached prompts and queue while others idle. `balancing` places each request by load instead, and keeps a conversation on its pod unless that pod is clearly the wrong place for it.

```yaml
spec:
  backends:
    - name: qwen
      inferenceServiceRef:
        name: qwen3-8-27b
      resolution: endpoint
      balancing:
        mode: LoadAware
        sessionAffinity:
          headers: [x-session-affinity, x-openwebui-chat-id, x-opencode-session, x-session-id]
          idleTimeout: 30m
```

`sessionAffinity` requires `mode: LoadAware`. The webhook rejects it with `mode: RoundRobin` when a router is created or its spec changes.

### How load is read

- **Load** for a vLLM backend is the pod's own running plus waiting requests, read from its `/metrics` once a second, plus the requests this proxy sent since. Other runtimes, and any pod whose metrics are more than 5 seconds old, use this proxy's in-flight count.
- **Prefill tokens** are this proxy's estimate of the uncached prompt tokens it has sent to a pod that the pod has not started answering. A request's estimate is its body size divided by 4, capped at 262,144. A known session's request counts only how much its body grew since the session's last request; a new or moved session counts its whole body. The tokens are released when the pod sends its first response byte, which for a streaming engine means prefill is done, or when the request fails or the client leaves.
- When this page says least-loaded, it means the lowest load counted in requests plus prefill tokens, with 16,384 tokens weighing as much as one request. So a new session, a request without a session header, a moved session and a spill target avoid a pod that is in the middle of a long cold prefill. Whether a known session moves is still decided by the pressure rules below, which use load in requests; prefill tokens only choose the target those rules are measured against. So a KV-full pod can occasionally shed a session it would otherwise have kept, always toward a pod with KV room.
- Like the in-flight count, prefill tokens are counted per proxy replica.
- `POST /v1/messages/count_tokens` carries the whole conversation but runs no prefill, so it counts no prefill tokens, is placed least-loaded without its session header, leaves the session's binding and size record unchanged, and is not counted in `llmkube_router_placements_total`.
- A pod whose KV cache is 90% full or more is KV-full. New sessions avoid KV-full pods while another pod has room.

### Where a session goes

Without a session header, each request goes to the least-loaded pod. With one, the first configured header present names the session. Only a hash of its value is kept.

- **A new session** goes to the least-loaded pod and is remembered there.
- **A known session stays on its pod** unless one of three things moves it. With fresh engine metrics, it moves if its pod has requests waiting and carries at least 4 more requests than the least-loaded pod. Without fresh metrics, it moves on an in-flight gap of 8 or more alone, with no waiting requests needed. It also moves if its pod is KV-full and queuing and the target has a fresh sample showing KV room, as long as the target is no busier than the full pod; a KV-full pod sheds only to a pod no busier than itself, so a herd cannot pile onto one target. If the target has no such sample, the gap of 4 still applies. A pod whose fresh sample is KV-full is never a spill target. When every pod is KV-full, a session whose pod left can still land on a full pod.
- **On a backend with fewer than 3 dispatchable pods, only a KV-full pod sheds sessions.** A queue and a load gap alone never move a session there, because with one or two pods every move lands on a cold prefix cache. Moving off a pod that left the candidate set is unchanged.
- **A session that moved stays on its new pod.** It cannot move again for pressure for 60 seconds, and after that it can.
- **A session whose pod left** (not ready, or quarantined by the circuit breaker) moves at once.
- **KV-full alone does not move a session.** Only new sessions avoid KV-full pods.
- **If the table is full** (100,000 sessions per backend), a new session falls back to a consistent-hash home pod and is not remembered.

`idleTimeout` drops a binding that has gone unused that long. The default is 30m, and the allowed range is 1m to 24h. Common agent headers are `x-session-affinity`, `x-openwebui-chat-id`, `x-opencode-session` and `x-session-id`.

### Running more than one proxy replica

Each proxy replica keeps its own binding table in memory. A restart starts it empty, and the table stores only the hash of the session header. With more than one proxy replica, a session that lands on different replicas gets different bindings. Hash the session header to a proxy replica upstream so a session always reaches the same replica, for example with APISIX `chash` on the header.

### Reading the metrics

`llmkube_router_placements_total{router,backend,result,reason}` counts decisions. `result` is one of:

- `least_loaded`: a request without a binding, or a new session, went to the least-loaded pod.
- `sticky`: a known session stayed on its pod.
- `spill`: a session moved off a pod under pressure (`waiting` or `kv_full`) and is now bound to the new pod.
- `migrated`: a session moved because its pod left the ready endpoint set (`not_ready`) or was still in that set but quarantined by the proxy (`quarantined`).
- `fallback`: the table was full and the consistent-hash home was used, or no pod was dispatchable (all quarantined or circuit-open) and the request went round-robin over the live endpoints. A fallback count alone does not mean the table is full.

`reason` is set only for `spill` and `migrated`, and empty otherwise. `llmkube_router_session_bindings{router,backend}` is the current number of bindings. It counts every session used within `idleTimeout`, not only live ones, so a client that starts a new session whenever one ends can hold several times its concurrent session count. Per pod, `llmkube_router_endpoint_load`, `llmkube_router_endpoint_metrics_fresh` and `llmkube_router_endpoint_prefill_tokens` cover `LoadAware` backends with a metrics profile (vLLM today). `llmkube_router_prefill_tokens_estimated_total` counts the prefill tokens this proxy estimated it sent to each pod; compare its rate with `rate(vllm:prompt_tokens_by_source_total{source="local_compute"}[1m])` on the same pod to check the estimate.

### Upgrading from `maxLoadPercent`

`sessionAffinity.maxLoadPercent` is deprecated and ignored. The webhook returns a warning when it is set, so remove it from your manifests. An existing `RoundRobin` router that has `sessionAffinity` keeps serving with its affinity ignored, and gets a `SessionAffinityIgnored` Warning event. Change it to `mode: LoadAware` to get sessions back.

Every LoadAware router created under 0.10.7 got `maxLoadPercent: 125` from the old CRD default, so the warning can fire even if your manifest never set it, and removing the field from the manifest does not clear it. Remove the stored value with a JSON patch, using the backend's index for `<i>`:

```bash
kubectl patch modelrouter <name> --type=json -p '[{"op":"remove","path":"/spec/backends/<i>/balancing/sessionAffinity/maxLoadPercent"}]'
```

A ModelRouter whose `spec.proxy.image` is pinned to a 0.10.7 proxy keeps the old ring behavior, and under the new controller any custom `maxLoadPercent` falls back to 125, because the controller no longer compiles it. Unpin the image.

## Several models on dataPlane: AgentGateway

With `dataPlane: AgentGateway`, each backend's `InferenceService` becomes an `InferencePool`, and agentgateway asks that pool's Endpoint Picker (EPP) which pod serves each request. A Gateway API Inference Extension EPP binds to exactly one pool, so a router that fronts several models gives each backend its own picker:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: b200-models
spec:
  dataPlane: AgentGateway
  agentGatewayRef:
    name: ai-gateway
  backends:
    - name: glm
      inferenceServiceRef:
        name: glm-53
      endpointPicker:
        service: {name: glm-epp, port: 9002}
    - name: qwen
      inferenceServiceRef:
        name: qwen-38
      endpointPicker:
        service: {name: qwen-epp, port: 9002}
  rules:
    - name: glm
      match: {models: [glm-5.3]}
      route: {backends: [glm]}
    - name: qwen
      match: {models: [qwen-3.8]}
      route: {backends: [qwen]}
```

`agentGatewayRef.endpointPicker` and `endpointPickerPort` remain the router-wide default for any backend without its own picker. The router reports `AgentGatewayReady=False` when:

- two pools would share one picker (`MultipleInferencePoolsUnsupported`), which is what the default does when backends reference more than one `InferenceService`; the same reason covers two `InferenceService` names that compile to one pool name, such as `glm.53` and `glm-53`;
- two backends on the same `InferenceService` name different pickers (`ConflictingEndpointPicker`);
- a pool has no picker at all (`EndpointPickerRequired`).

The pickers are yours to run; LLMKube references them by Service name and port in the router's namespace. A backend without its own picker uses the picker another backend on the same `InferenceService` names, and only then the router default.

Each `InferencePool` is named after its `InferenceService`, and a router never adopts a pool another router created. When you move from one router per model to one router for several, delete the per-model routers first: until their pools are garbage-collected, the combined router reports `GeneratedResourceNameTaken`.

`primary-fallback` routes each rule to its first healthy backend. When that backend's `InferenceService` has no ready replica, the next reconcile moves the rule to the next backend. Failover follows replica readiness, not individual request errors: a primary that is up but failing requests keeps its traffic.

## Model routing on dataPlane: AgentGateway

`agentGatewayRef.routing: Model` compiles the router onto agentgateway's own model router instead of the Inference Extension. It needs agentgateway v1.6.0 or later, which ships the `AgentgatewayModel` CRD, and a Gateway. It needs no Gateway API Inference Extension CRDs, no Endpoint Picker, and no Gateway-wide policy, which suits single-replica `InferenceService`s reached through their Service.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelRouter
metadata:
  name: llm-router
spec:
  dataPlane: AgentGateway
  agentGatewayRef:
    name: ai-gateway
    routing: Model
    sectionName: llm
  backends:
    - name: chat
      displayName: chat-large
      inferenceServiceRef:
        name: chat-vllm
    - name: chat-small
      inferenceServiceRef:
        name: chat-llama
    - name: reranker
      inferenceServiceRef:
        name: reranker
```

`chat-vllm` must start vLLM with `--served-model-name=chat-large` in its `extraArgs`: without it, LLMKube starts vLLM serving the InferenceService's `spec.modelRef` (and the model path), not `chat-large`.

The router generates, all in its own namespace and owned by it:

- one `HTTPRoute` named after the router, attached to the Gateway, with a single rule named `models` at `/` whose only backend is `AgentgatewayModel` `*`. The rule sits at `/` because agentgateway strips the rule's path prefix before it classifies the endpoint: under `/v1`, `/v1/embeddings` would parse as a chat completion;
- one `AgentgatewayModel` per backend, named `<router>-<backend>` (cut and hashed past 63 characters), attached to that rule. Its `match.model` is the backend's `displayName`, else its `name`, so served names such as `Qwen3.5-2B` or `org/model` work. It reaches the `InferenceService`'s Service through a custom provider, and the request keeps its model name, so the served name must be one the runtime answers to: llama.cpp ignores it, and vLLM answers to its `--served-model-name` from `extraArgs`, else to `spec.modelRef`.

The provider and API formats come from the `InferenceService`:

| Runtime | `spec.mode` (or inferred) | `providerOverride` | Formats |
|---|---|---|---|
| `llamacpp` (or unset) | chat | `llamacpp` | `Completions` |
| `vllm` | chat | `vllm` | `Completions`, `Messages`, `AnthropicTokenCount`, `Responses` |
| `llamacpp`, `vllm` | embedding | as above | `Embeddings` |
| `llamacpp`, `vllm` | rerank | as above | `Rerank` |

agentgateway then routes each request on the body's `model`, answers `GET /v1/models` itself, and returns a JSON 404 `model_not_found` for a model no backend serves.

A rule at `/` takes every path on the listeners it attaches to, so this mode requires `agentGatewayRef.sectionName` (one listener), `agentGatewayRef.hostnames`, or both. Your own routes still share the listener: a rule with a longer match, such as a `PathPrefix` `/v1` with a header match, outranks the router's `/`. Use one `routing: Model` router per listener and hostname. Two routers' `/` rules tie, Gateway API gives the tie to the older route, and the newer router's models get no traffic.

The router reports `AgentGatewayReady=False` and generates nothing when:

- the `AgentgatewayModel` CRD is not installed (`AgentgatewayModelCRDNotInstalled`);
- the spec has `rules`, a `defaultRoute`, an `external` backend, `resolution: endpoint`, a backend `endpointPicker`, a served name containing `*`, or two backends serving one name (`UnsupportedInModelRouting`; the webhook rejects these at apply time). Fallback between backends and external backends need an `AgentgatewayBackend`, which this mode does not generate yet;
- a backend's `InferenceService` runs a runtime other than `llamacpp` or `vllm`, or runs vLLM serving a name other than the backend's served name (`UnsupportedRuntimeInModelRouting`);
- a generated name is held by an object LLMKube did not create (`GeneratedResourceNameTaken`); delete or rename that object.

When you replace hand-written `AgentgatewayModel`s with a router, remove the hand-written ones for the same model names on that listener, so each name has one model.

Removing a backend deletes its model. Switching `routing` in either direction deletes the other mode's objects, including the `InferencePool` mode's extraction policy in the Gateway's namespace.

## Composition with LiteLLM

LLMKube does not replace [LiteLLM](https://github.com/BerriAI/litellm). For organizations already running a LiteLLM proxy as their cloud-provider abstraction, point a `ModelRouter` external backend at it:

```yaml
- name: anything-via-litellm
  tier: cloud
  external:
    provider: litellm
    url: http://foundation-router.gateway.svc.cluster.local:4000
    model: openrouter/anthropic/claude-opus-4-7
    credentialsSecretRef:
      name: litellm-master-key
```

LiteLLM handles provider auth, retries, and cost tracking. ModelRouter adds K8s-native policy, fail-closed enforcement, and audit logs.

### Cluster-wide LiteLLM default

Platform teams can centralize the LiteLLM URL via a controller flag so application teams don't have to repeat it on every `ModelRouter`:

```yaml
# Helm values
controllerManager:
  routerProxy:
    defaultLiteLLMURL: http://litellm.litellm.svc.cluster.local:4000
```

With that set, application teams can declare LiteLLM-backed routers without `url`:

```yaml
external:
  provider: litellm
  model: openrouter/anthropic/claude-opus-4-7
```

A per-backend `url` always wins over the cluster default.

## Shape compatibility

The router-proxy forwards the inbound OpenAI chat-completion request body to upstream backends verbatim. For backends that already speak OpenAI (local `InferenceService` pods, LiteLLM, an in-cluster vLLM or oMLX), this is correct. For first-party providers whose native API does not match (Anthropic's `/v1/messages`, Bedrock's per-region shapes, Vertex AI's structured Content payloads), put a LiteLLM proxy in front and reference it via `provider: litellm` — LiteLLM handles the per-provider translation.

When you specify `provider: anthropic` or `provider: openai` without `url`, the controller fills in the published default (`https://api.anthropic.com`, `https://api.openai.com`). This works as-is for OpenAI (which already speaks the OpenAI shape) and for any Anthropic-compatible endpoint that accepts OpenAI requests. Direct calls against `api.anthropic.com` itself require LiteLLM in front; native Anthropic-Messages translation is on the roadmap, not in Phase 1.

## What's in scope, what isn't

**In scope:**

- Routing rules: data classification, task complexity, required capabilities, model glob, header match
- Strategies: primary-fallback and weighted; shadow (roadmap)
- Fail-closed gate, both static (apply-time) and runtime
- OpenAI-compatible request / response, including streaming SSE
- Structured audit log to stdout (other sinks roadmap)
- Per-route budget caps (roadmap)
- MCP server endpoint (roadmap)

**Out of scope:**

- The agent runtime itself. ModelRouter is consumed *by* LangGraph, CrewAI, OpenAI Agents SDK, Anthropic Agent SDK, Cline, OpenCode, Aider, and any other framework that speaks the OpenAI API. We don't reinvent that layer.
- Inference engines. `InferenceService` already wraps llama.cpp, vLLM, TGI, oMLX. ModelRouter sits *above* them.
- General-purpose K8s gateway. ModelRouter is scoped specifically to LLM traffic with policy.

## Phase 1 limitations (v1alpha1)

The Phase 1 router-proxy ships with three concrete gaps that show up
on agentic-coding workloads. They are intentional scope for v1alpha1
and tracked for Phase 2; calling them out so you can plan around them
rather than discovering them mid-incident.

### 1. Timeouts cap TTFT, not stream duration

`rule.timeout` and `backend.timeout` apply to the **time-to-first-byte**
(first response header from the upstream), not the total duration of
the stream. Once the upstream starts sending SSE chunks, the proxy
will pipe them to your client for as long as the upstream keeps
producing, with no aggregate cap.

For agentic-coding workloads this is usually what you want (large
refactors generate 5-10 minute SSE streams that you don't want the
proxy interrupting), but it means a *hung* stream where the upstream
goes silent mid-response is bounded only by kernel TCP keepalive or
your client's read deadline. **Set a client-side timeout in your
agent runtime as the safety net for hung streams.** A stream-duration
cap field is planned for Phase 2.

### 2. Audit log is coarse

The per-request audit-log line records `latencyMs`, `status`,
`outcome`, and `timeoutMs`, but not streamed bytes, token count, or
stream-duration breakdown. "Why did this 8-minute agent loop run
slow?" needs upstream metrics, not the proxy log.

`outcome` says how a dispatched request ended:

- `ok` or `ok_stream`: the response was relayed in full.
- `client_cancelled`: the client disconnected. If no backend had
  answered yet, the line records status `499` and no fallback is tried.
- `upstream_error`: the backend broke off mid-response.
- `timeout`: the backend missed the header deadline (on a rule that is
  not fail-closed, the client gets a `504`), or a stream went silent past
  `streamIdleTimeout`.
- `all_backends_failed`: every backend errored or returned a 5xx (`502`).

The failure outcomes carry the underlying `error`. For a response the
proxy relayed, the same value labels `llmkube_router_requests_total`.
The proxy cancels the upstream request when its own request ends, so a
`cancel task` line in llama.cpp's log usually pairs with a
`client_cancelled` or `timeout` line here.

To see who sent each request, opt in under `spec.policy.auditLog`:

```yaml
spec:
  policy:
    auditLog:
      logClientAddress: true
      trustedProxyCIDRs: ["10.0.0.0/8"]  # only behind an ingress or gateway
      callerHeader: X-LLMKube-Client
```

- `logClientAddress` adds `client`, the sender's address.
  - In-cluster callers keep their pod IP through the ClusterIP.
  - The backend only ever sees the proxy's IP, so llama.cpp's own log can't tell you this.
  - It's off by default because client addresses are personal data in some deployments.
- `trustedProxyCIDRs` names the proxies in front of the router whose `X-Forwarded-For` is believed.
  - When the TCP peer is one of them, `client` is the first address in `X-Forwarded-For`, read from the right, that is not a trusted proxy.
  - Any client can send `X-Forwarded-For`, so without this list the header is ignored.
  - Entries are CIDRs or single addresses, and the field requires `logClientAddress`.
- `callerHeader` adds `caller`, the value of a header each client sets.
  - The client chooses the value, so treat it as a label, not an identity.
  - It's cut to 128 bytes, and characters outside printable ASCII become `?`.
  - Headers that carry a credential (`Authorization`, `Proxy-Authorization`, `Cookie`, `X-Api-Key`, `Api-Key`, `X-Goog-Api-Key`) are refused.

The proxy doesn't look up pod names: resolving an IP to a pod would need read access to every pod in the cluster. On Cilium, `hubble observe --to-pod <namespace>/<router>-router-proxy --port 8080` shows the calling pods.

Per-request token/byte accounting and a proxy-emitted Prometheus
histogram are planned for Phase 2 (issue [#433](https://github.com/defilantech/LLMKube/issues/433)).

### 3. Inbound request bodies are buffered

Outbound responses stream chunk-by-chunk; **inbound requests are
fully buffered** before routing (up to a 32 MiB cap, enough for
~128K-token prompts). A request's body is released once it has been
sent upstream, so a reply that streams for minutes does not keep its
prompt in memory. While a request is being sent, the proxy holds
about two to three times its body size when it rewrites the model
name. A long-context agent prompt of 75K tokens is about 300 KB of
JSON, so a few hundred such requests arriving together need a few
hundred MiB. The default limit is 1 GiB (request 64 MiB); raise
`spec.proxy.resources` for larger bursts. True request-side
streaming is planned for Phase 2.

## Comparison to alternatives

| | ModelRouter | LiteLLM proxy | KubeAI Model Proxy | llm-d Inference Gateway |
|---|---|---|---|---|
| **K8s-native CRD** | ✓ | — (Helm chart, no CRD) | ✓ (limited) | ✓ (Gateway API extension) |
| **Cross-engine handoff (local + cloud)** | ✓ | ✓ (cloud only) | — (local intra-cluster only) | — (vLLM-focused) |
| **Fail-closed for sensitive data** | ✓ (static + runtime) | — | — | — |
| **Audit log per request** | ✓ | partial | — | partial |
| **Composes with LiteLLM** | ✓ (as a backend) | n/a | — | — |

The three peers all solve adjacent but different problems. ModelRouter's specific niche is **policy-aware hybrid routing for regulated-industry adoption**: the place where "I run my own AI" meets "I sometimes need to call Opus" meets "compliance must be enforceable."

## Status surface

After a successful reconcile, `status` on a ModelRouter looks like:

```yaml
status:
  phase: Provisioning   # or Ready / Degraded / Failed
  endpoint: http://coding-router-router-proxy.default.svc.cluster.local:8080/v1/chat/completions
  activeRules: 2
  backends:
    - name: local-qwen
      tier: local
      address: http://qwen3-coder.default.svc.cluster.local:8080
      healthy: true
    - name: cloud-opus
      tier: cloud
      address: https://api.anthropic.com
      healthy: true
  conditions:
    - type: Validated
      status: "True"
      reason: SpecValid
    - type: BackendsReady
      status: "True"
      reason: BackendsResolved
    - type: Available
      status: "True"
      reason: DeploymentReady
```

`phase` is the coarse summary; the conditions tell the full story.

When the data plane cannot serve, `Available` flips to `False` with one of these reasons:

| Reason | Meaning |
| --- | --- |
| `CompileFailed` | The routing spec did not compile into a proxy config. |
| `ReconcileFailed` | Applying the ConfigMap, Deployment, Services, or activation RBAC to the API server failed. |
| `RouterProxyNameTaken` | An object LLMKube did not create already holds a generated router-proxy name (the ConfigMap, Deployment, Services, ServiceAccount, Role, or RoleBinding named after the router). The operator refuses to adopt or overwrite it; delete or rename the foreign object, or the router, and the reconcile recovers on its next pass, rechecked about every 2 minutes. |
| `DeploymentNotReady` | The proxy Deployment exists but its pods are not Ready yet. |

## Next steps

- Read the [CRD reference](/docs/concepts/crds) for the full spec.
- See [Multi-GPU sharding](/docs/guides/multi-gpu) for backing the local InferenceService with sharded GPU pods.
- Read the [comparison page](/docs/concepts/comparison) to understand how ModelRouter fits relative to other K8s LLM operators.

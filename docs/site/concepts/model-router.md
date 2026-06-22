---
title: Model Router
description: A single OpenAI-compatible endpoint that routes across local InferenceServices and external providers (Anthropic, OpenAI, LiteLLM) with policy-driven handoff, fail-closed semantics for regulated data, and audit log per request.
updated: 2026-05-12
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

<DocCallout variant="warn" title="Router-proxy image not yet in the release pipeline">

The release pipeline currently builds and publishes the controller image (`ghcr.io/defilantech/llmkube-controller`) on every release. The router-proxy image (`ghcr.io/defilantech/llmkube-router-proxy`) is not yet built by CI: see [issue #449](https://github.com/defilantech/LLMKube/issues/449).

The Helm chart's default for `controllerManager.routerProxy.tag` is `"dev"` until that lands. Users who want to deploy a ModelRouter today have two options:

1. **Build locally and push.** Run `make docker-build-router-proxy ROUTER_PROXY_IMG=<your-registry>/llmkube-router-proxy:dev`, push it, then `--set controllerManager.routerProxy.repository=<your-registry>/llmkube-router-proxy` on the Helm install.
2. **Override per ModelRouter.** Set `spec.proxy.image` on each ModelRouter to an image your cluster can pull.

Users who never create a ModelRouter resource are unaffected.

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

Since every spec change re-rolls the pods, set `spec.proxy.revisionHistoryLimit` to cap how many old `ReplicaSet`s are kept (`0` keeps none); unset uses the Kubernetes default of 10. `InferenceService` has the same knob at `spec.revisionHistoryLimit`.

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

Point any OpenAI-compatible client at that URL and it just works. Headers that change routing:

| Header | Effect |
|---|---|
| `x-llmkube-classification: pii` | Matches `pii-stays-local` rule; local-only, fail-closed. |
| `x-llmkube-task-complexity: complex` | Matches `complex-to-cloud` rule; tries Opus first, falls back to Qwen on 5xx. |
| (no headers) | Falls through to `defaultRoute: local-qwen`. |

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
- Strategies: primary-fallback (MVP), weighted and shadow (roadmap)
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

Per-request token/byte accounting and a proxy-emitted Prometheus
histogram are planned for Phase 2 (issue [#433](https://github.com/defilantech/LLMKube/issues/433)).

### 3. Inbound request bodies are buffered

Outbound responses stream chunk-by-chunk; **inbound requests are
fully buffered** before routing (up to a 32 MiB cap, enough for
~128K-token prompts). For 50 concurrent long-context requests that
is around 25 MB of resident memory in the proxy pod, comfortable on
a default 256 MiB pod but not zero. True request-side streaming is
planned for Phase 2.

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

## Next steps

- Read the [CRD reference](/docs/concepts/crds) for the full spec.
- See [Multi-GPU sharding](/docs/guides/multi-gpu) for backing the local InferenceService with sharded GPU pods.
- Read the [comparison page](/docs/concepts/comparison) to understand how ModelRouter fits relative to other K8s LLM operators.

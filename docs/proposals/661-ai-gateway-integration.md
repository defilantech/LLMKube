# Proposal: First-class Envoy AI Gateway integration

**Status:** Accepted (design); MVP in progress
**Umbrella issue:** [#661](https://github.com/defilantech/LLMKube/issues/661)
**Related:** [#662](https://github.com/defilantech/LLMKube/issues/662) (metal-agent endpoint health)
**Validated against:** Envoy Gateway v1.8.1, Envoy AI Gateway v0.7.0, Gateway API Inference Extension (GIE) v1.0.x, with llama.cpp backends across in-cluster CUDA pods and off-cluster Metal hosts.

This document is the canonical design reference for the AI Gateway integration epic. It captures the north-star architecture, the sub-project decomposition, the detailed MVP design, and, critically, the expansion path so that each deferred slice slots into a stable, written architecture without re-deriving it.

---

## 1. Problem and motivation

The Envoy AI Gateway ecosystem (Envoy Gateway + Envoy AI Gateway + the Gateway API Inference Extension) is excellent at moving bytes: JWT auth, per-team model allowlists, exact streamed token metering, token budgets and 429 enforcement, audit access logs, and HA failover all work end to end with zero custom data-plane code. What it does not know is **models as workloads**. It cannot see that a backend is still downloading a model, is GPU-queued, or that one logical model is served across heterogeneous tiers (in-cluster CUDA pods plus off-cluster Metal hosts). LLMKube owns exactly that state. This integration is the bridge.

Two concrete pains motivate the work:

1. **Hand-written resource sprawl.** Fronting a fleet today requires hand-authoring roughly eight resource kinds per deployment (`Gateway`, `EnvoyProxy`, `AIGatewayRoute`, `Backend` + `AIServiceBackend` pairs, `SecurityPolicy`, `BackendTrafficPolicy`, `InferencePool` + endpoint-picker `Deployment`), with real footguns (see Section 7). This does not scale past a couple of models.

2. **Backend-lifecycle blindness.** An abruptly killed backend behind a ClusterIP-resolved `Backend` stalls in-flight requests for the full per-attempt timeout (60s measured) with no failover, while pod-backed `InferencePool` endpoints fail new requests over in 2-4ms. Off-cluster Metal endpoints have no pool equivalent (pools are pods-only), so half a heterogeneous fleet gets the slow path.

## 2. North-star architecture

**Adopt the Envoy AI Gateway as the data plane. Build the LLM-aware control plane as LLMKube-side work, not custom Envoy filters.** Every gap found in the homelab spike is control-plane-shaped, which is exactly what a Kubernetes operator is for.

```
        user-facing policy CRDs                 generated Gateway API / aigw resources
   ┌───────────────────────────────┐        ┌──────────────────────────────────────────┐
   │ InferenceService.spec.endpoint │        │ Backend + AIServiceBackend + AIGatewayRoute│
   │   .gateway   (this proposal)   │ ─────► │ (+ later: InferencePool, SecurityPolicy,   │
   │ ModelRouter.spec.dataPlane:    │  LLMKube │      BackendTrafficPolicy, ...)            │
   │   Gateway    (later slice)     │ operator └──────────────────────────────────────────┘
   └───────────────────────────────┘                         │ programs
                                                              ▼
                                              ┌──────────────────────────────────────────┐
                                              │   Envoy AI Gateway data plane (adopted)    │
                                              │   auth · metering · budgets · audit · HA   │
                                              └──────────────────────────────────────────┘
```

The eventual contract: **ModelRouter becomes the control plane that programs the gateway** via a new `spec.dataPlane: Gateway` mode (alongside the existing `Proxy`/router-proxy mode during transition), and InferenceService gains opt-in gateway exposure. `router-proxy` mode deprecates once Gateway mode reaches parity.

## 3. Epic decomposition

The epic splits into four independently shippable sub-projects. Each gets its own spec -> plan -> implementation cycle.

| # | Sub-project | What it delivers | Depends on |
|---|-------------|------------------|------------|
| **1** | **InferenceService gateway exposure** (this proposal's MVP) | Operator generates + lifecycle-binds `Backend` + `AIServiceBackend` + `AIGatewayRoute` per opted-in InferenceService | — |
| 2 | ModelRouter `dataPlane: Gateway` compiler | Compile ModelRouter policy (rules, budgets, audit) into `AIGatewayRoute`/`SecurityPolicy`/`BackendTrafficPolicy`, footguns made structurally impossible | 1 |
| 3 | metal-agent gateway-aware endpoint health ([#662](https://github.com/defilantech/LLMKube/issues/662)) | Agent ejects/restores its managed Endpoints address on health change + surfaces status | — (agent-side) |
| 4 | Backend health bridging | Operator compiles health (3 + pod health) into gateway `Backend` eject/restore for event-driven failover | 1, 3 |

**Sequence:** 1 is the foundation. 2 and (3 -> 4) build on it. 3 is agent-side and can proceed in parallel; 4 consumes it.

---

## 4. MVP design (sub-project 1): InferenceService gateway exposure

### 4.1 Boundary and assumptions

- The Envoy AI Gateway stack (Envoy Gateway, Envoy AI Gateway, their CRDs) and a `Gateway` + listener are **pre-installed and referenced**, not installed or owned by LLMKube. Managing the gateway install is out of scope.
- **Feature-gated.** The operator reconciles gateway resources only when (a) the InferenceService opts in and (b) the aigw + Gateway API CRDs are present (detected via RESTMapper/discovery at startup). CRDs absent -> graceful no-op with a clear status condition. This mirrors the optional validating-webhook pattern (#520).
- Both tiers (in-cluster CUDA pods, off-cluster Metal Macs) expose a Kubernetes `Service` named after the InferenceService (operator-created for pods; metal-agent's selectorless Service for Macs). The generated `Backend` targets that Service uniformly, so the MVP needs **no tier branching**.

### 4.2 API: opt-in on InferenceService

A new block under the existing `spec.endpoint`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
spec:
  modelRef: qwen36-35b
  endpoint:
    gateway:
      enabled: true
      gatewayRef:
        name: ai-gateway
        namespace: ai-gateway
      modelName: qwen36-35b   # optional; the OpenAI `model` string clients send.
                              # Defaults from modelRef / the InferenceService name.
```

`gateway` is `*GatewaySpec` (nil = no exposure, preserving today's behavior). Fields:
- `enabled bool` — opt-in switch.
- `gatewayRef {name, namespace}` — the `Gateway` to attach the route to.
- `modelName string` (optional) — the model-name match value; defaults as above.

### 4.3 Resources generated (per opted-in InferenceService)

All owner-referenced to the InferenceService (automatic GC on delete), reconciled with `CreateOrUpdate` (idempotent, drift-correcting):

1. **`Backend`** (`gateway.envoyproxy.io`) -> the InferenceService's `Service`.
2. **`AIServiceBackend`** (`aigateway.envoyproxy.io`) -> wraps the `Backend` with the OpenAI schema + model name.
3. **`AIGatewayRoute`** (`aigateway.envoyproxy.io`), **one per InferenceService**, attached to the referenced `Gateway`, a single rule: model-name match -> the `AIServiceBackend`.

**Decision: one `AIGatewayRoute` per InferenceService** (not one shared route with appended rules). Rationale: clean lifecycle/GC (the route dies with its InferenceService) and it sidesteps the spike footgun where the oldest route's catch-all rule gates auth for the whole listener (Section 7).

### 4.4 Controller

A new `InferenceServiceGatewayReconciler` (separate from the core InferenceService controller, so the integration stays cleanly optional and feature-flaggable), watching InferenceServices, gated on opt-in + CRD presence.

- **CRD-presence detection** at startup via RESTMapper. Absent -> log "gateway integration disabled (CRDs not installed)" and skip; never crash.
- **Status:** set `status.gateway` on the InferenceService (route ready + resolved endpoint) so users can see the exposure worked.
- **RBAC:** kubebuilder markers granting create/update/delete/get/list/watch on the new `Backend`/`AIServiceBackend`/`AIGatewayRoute` kinds; chart RBAC synced; `make check-helm-rbac` green.

### 4.5 Cross-namespace attachment

The generated route lives in the InferenceService namespace and attaches to a `Gateway` in the gateway namespace. **For the MVP, the Gateway listener's `allowedRoutes.namespaces` is a documented prerequisite** (the operator does not auto-configure the listener and does not generate `ReferenceGrant`s). This keeps the MVP from reaching into gateway-owned config. Auto-managing cross-namespace permission is a candidate follow-up.

### 4.6 Testing

- envtest with the aigw + Gateway API CRDs vendored in: assert an opted-in InferenceService produces the three resources with correct owner refs + model match; that InferenceService delete GCs them; and that **CRDs-absent is a clean no-op**.
- No live data-plane test in CI; data-plane behavior (auth, metering, failover, overhead) is validated in the homelab/e2e realm, where the spike already measured it.

### 4.7 Explicitly deferred (keeps the MVP tight)

- InferencePool + endpoint-picker fast-path (Section 5.1).
- SecurityPolicy/auth, BackendTrafficPolicy/budgets, audit -> ModelRouter `dataPlane: Gateway` compiler (Section 5.2).
- Backend health eject/restore (#662, Section 5.3).
- Cross-tier fallback under one model name (blocked upstream, Section 7).
- Managing the gateway install itself.

---

## 5. Expansion path (how the deferred slices extend the MVP)

The MVP is deliberately the stable spine the rest hangs off. Each later slice is additive.

### 5.1 InferencePool fast-path (pod runtimes)
For in-cluster pod-backed runtimes, additionally generate an `InferencePool` (`inference.networking.k8s.io`) + endpoint-picker (EPP) `Deployment`, and point the route rule at the pool instead of the `AIServiceBackend`. Delivers 2-4ms event-driven failover for pods. Brings in the **namespace-locked pool ref** constraint (the pool must live in the pod namespace; Section 7) and the EPP deployment lifecycle. Opt-in via something like `spec.endpoint.gateway.inferencePool: true`. The MVP's `Backend`/`AIServiceBackend` generation stays the fallback/Metal path.

### 5.2 ModelRouter `dataPlane: Gateway` compiler (sub-project 2)
ModelRouter stays the user-facing policy surface (rules, budgets, classification, audit). A new `spec.dataPlane: Gateway` mode compiles that policy onto the routes the MVP already generates: emit `SecurityPolicy` (JWT/auth + per-team model allowlists), `BackendTrafficPolicy` (budgets + retry/fallback, kept in ONE BTP per Section 7), and audit config. The compiler is where the footguns are made structurally impossible. router-proxy `Proxy` mode coexists during transition and deprecates at parity; the fail-closed classification path (PII) needs a design check against ext_authz/ext_proc before parity is claimed.

### 5.3 Backend health bridging (sub-projects 3 + 4)
metal-agent (#662) ejects/restores the address on its managed Endpoints object on health change and surfaces status. The operator (sub-project 4) compiles that health, plus pod health, into gateway `Backend` ejection/restoration, giving off-cluster Metal endpoints the event-driven detection that pods get from the EPP. This mutates the `Backend`s the MVP generates.

---

## 6. RBAC, versions, and risks

- **Versions** are pinned by the spike: Envoy Gateway v1.8.1, Envoy AI Gateway v0.7.0, GIE v1.0.x CRDs. Envoy AI Gateway is young (v0.7.0); generated resource shapes may need updates as it evolves. Pin the tested versions and track upstream.
- **Upstream engagement** is part of the adopt posture: two candidate upstream issues exist (cross-namespace pool refs broken; no pool/AIServiceBackend mixing for fallback).
- **Risk:** cross-namespace route attachment depends on gateway-side `allowedRoutes` config (documented prerequisite for the MVP). Optional-CRD detection must be robust (never crash when the gateway is absent).

## 7. Upstream constraints and footguns to design around

Discovered and reproduced during the homelab spike; these shape the later slices:

1. **Retry/fallback and rate limiting must live in ONE `BackendTrafficPolicy`** or the newer policy silently no-ops. The ModelRouter compiler (5.2) must emit a single combined BTP.
2. **`InferencePool` cannot mix with `AIServiceBackend` refs in one rule, and cannot carry priorities** (v0.7.0). Pool-based routing and Metal fallback under one model name are mutually exclusive today -> no cross-tier fallback under one name without health-driven route mutation.
3. **`InferencePool` refs are namespace-locked** to their pods. Shapes the 5.1 fast-path (pool in the pod namespace, route in the gateway namespace).
4. **The oldest route's catch-all rule gates auth for the whole listener.** Avoided in the MVP by one-route-per-InferenceService (4.3).
5. **Token metadata requires `llmRequestCosts`**; JWKS rotation has a cache window; a webhook race can occur on controller upgrades. Relevant to the metering/auth slice (5.2).

## 8. Open questions

- Should the operator eventually auto-manage cross-namespace permission (`ReferenceGrant` / listener `allowedRoutes`) rather than treating it as a prerequisite?
- Where does the fail-closed PII classification path land in the Gateway data plane (ext_authz vs ext_proc) for ModelRouter parity?
- How to express cross-tier (CUDA + Metal) fallback under one model name given the upstream pool/backend mixing constraint: upstream contribution, or LLMKube-side health-driven route mutation?

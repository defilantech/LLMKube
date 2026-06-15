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
ModelRouter stays the user-facing policy surface (rules, budgets, classification, audit). A new `spec.dataPlane: Gateway` mode (default `Proxy`, fully back-compat) compiles that policy onto the routes the MVP already generates. The compiler is where the footguns are made structurally impossible. router-proxy `Proxy` mode coexists during transition and deprecates at parity. Sub-project 2 is itself a mini-epic, sliced so each layer lands reviewably onto the routes the prior layer produced:

| Sub-slice | Compiles | Notes |
|-----------|----------|-------|
| **2a** | `dataPlane: Gateway` mode + `backends` + `rules` -> `Backend`/`AIServiceBackend` + a multi-rule `AIGatewayRoute` + a retry/failover `BackendTrafficPolicy` | The heterogeneous-tier failover differentiator |
| 2b | `policy.budgets` -> token/dollar caps + 429 enforcement, added to the SAME BTP 2a created (footgun 7.1) | |
| 2c | `policy.auditLog` -> EnvoyProxy access-log config to a sink (Loki) | |
| 2d | identity/team allowlists (new ModelRouter auth surface) -> `SecurityPolicy` | |
| 2e | `policy.classification` + `failClosed` (PII) -> ext_authz/ext_proc | parity gate; designed last |

#### Sub-slice 2a (detailed design)

- **Mode.** Add `spec.dataPlane: Proxy | Gateway` (default `Proxy`; existing router-proxy behavior unchanged) and `spec.gatewayRef` (reuses the slice-1 `GatewayReference`). In `Gateway` mode the operator compiles gateway resources instead of provisioning router-proxy.
- **Controller.** A separate `ModelRouterGatewayReconciler`, CRD-presence-gated (reusing slice 1's detection), owns the `dataPlane: Gateway` path. The existing ModelRouter controller gets a single guard: skip router-proxy provisioning when `dataPlane: Gateway` (the only touch to existing behavior; guarded, back-compat since the default is `Proxy`). Switching an existing ModelRouter from `Proxy` to `Gateway` cleaning up the old router-proxy Deployment is a documented follow-up (parallels the slice-1 opt-out note).
- **Backends.** Each `InferenceServiceRef` backend compiles to a `Backend` + `AIServiceBackend` via slice 1's `gateway_resources.go` builders. External (cloud) backends are deferred from 2a (they need credential-secret + external-Backend shapes).
- **Rules + failover.** One `AIGatewayRoute` for the ModelRouter, a rule per `spec.rules` entry matching on model name and headers, routing to the rule's backends. Multi-backend failover compiles to multiple `backendRefs` carrying `priority` (failover) or `weight` (weighted), PLUS a `BackendTrafficPolicy` targeting the generated HTTPRoute (which shares the route name) carrying the `retry` config (`numAttemptsPerPriority`, `retryOn: [connect-failure, retriable-status-codes]`) and passive outlier `healthCheck` that the spike validated for two-tier CUDA+Metal failover. Without that BTP the route lists the backends but does not retry onto the secondary. 2b adds the token budget to THIS SAME BTP (footgun 7.1). `defaultRoute` compiles to a trailing catch-all rule.
- **Honest boundary (fail-loud).** The gateway data plane can match model name and headers, but NOT `dataClassification`, `taskComplexity`, `requiredCapabilities`, or `latencySLO` (those need request-body inspection / router-side scoring, which is slice 2e). A ModelRouter rule using any of those in `Gateway` mode sets the ModelRouter condition `False` (reason `UnsupportedMatchInGatewayMode`) and generates NOTHING, rather than silently compiling a partial route that drops a rule the user expects. A validating webhook to reject this at apply time is a follow-up.
- **Status.** `status.gateway` on the ModelRouter (routes ready + resolved endpoint) plus a condition.

#### Sub-slice 2b (detailed design): token budgets and 429 enforcement

Compile `policy.budgets[]` into token-denominated rate limiting on the resources 2a already generates.

- **One BTP, extended in place.** The `rateLimit` stanza is added to the SAME `BackendTrafficPolicy` 2a created for retry/failover, NOT a new policy. This is footgun 7.1 made structural: two `BackendTrafficPolicy` objects targeting one route silently conflict (the oldest wins), so retry and rate limit must share one BTP. The 2b builder extends the 2a BTP rather than emitting a second one.
- **Token-denominated.** Add `llmRequestCosts` (TotalToken) to the `AIGatewayRoute` 2a generates so the limit is charged in tokens at response completion, with the request path check-only. This is the mechanism the spike validated.
- **Scopes compiled in 2b: `router` and `team`.** `router` becomes a global rateLimit descriptor (applies to all traffic through the ModelRouter). `team` becomes a descriptor keyed on `BudgetSpec.HeaderKey` (default `x-llmkube-team`), so each distinct header value gets its own bucket. Each budget compiles `MaxTokens` over `WindowSeconds` into a rateLimit rule; exceeding it yields instant 429s with budget-countdown headers.

**Deliberately deferred, with the reasoning (so a later slice picks up a decided question, not an open one):**

- **`MaxUSD` (dollar budgets) -> fail-loud (`UnsupportedBudgetField`) until a dedicated follow-up.** The gateway rate-limits on tokens, so a dollar cap must convert to a token-equivalent. With heterogeneous backends at different `CostPerMillionTokens` under one rule, there is no single honest conversion (which backend's price sets the rate?). Rather than silently pick one and under- or over-charge, 2b ships `MaxTokens` enforcement and fails loud on `MaxUSD`. The conversion semantics (per-backend, blended, or worst-case-conservative) are a design question that gets its own slice once chosen.
- **`rule` scope -> fail-loud (`UnsupportedBudgetScope`) until a follow-up.** `router` and `team` are the high-value cases (total cap, per-tenant cap). `rule`-scoped budgets need a per-rule rateLimit clientSelector keyed on the rule's model match, which is fiddlier; deferring it keeps 2b tight and is reversible.
- **`team` scope is compiled now but documented as needing auth to be tamper-proof.** A `team` budget keys on whatever populates `HeaderKey`; without auth (slice 2d) a client can spoof that header for a fresh bucket. We compile it anyway (rather than block on 2d) because the rateLimit-by-header mechanism is identity-agnostic and the budget feature is useful immediately; 2d later derives the header from a verified JWT claim, which makes the same budget tamper-proof. The status/docs note this pairing so an operator is not misled into thinking header-keyed budgets are secure on their own.

- **Status.** Surface compiled budgets and any fail-loud reason (`UnsupportedBudgetField` / `UnsupportedBudgetScope`) on the ModelRouter condition, consistent with 2a's honest-boundary handling.

#### Sub-slice 2d-core (detailed design): JWT authentication

Add a new auth surface to ModelRouter and compile it into a `SecurityPolicy` attached to the route 2a generates. 2d-core is **authentication only**; per-team model allowlists (authorization) are slice 2d.2 (reasoning below).

- **API surface (`spec.policy.auth.jwt`).** New optional block: `provider` (a name), `issuer` (the OIDC issuer URL), `jwksURI` (the remote JWKS endpoint), and `teamClaim` (the JWT claim that identifies the tenant). Optional `headerKey` to override where the claim lands (defaults to `x-llmkube-team`, matching the 2b budget default).
- **Compiles to a `SecurityPolicy`** (`gateway.envoyproxy.io`) with `targetRefs` -> the generated HTTPRoute (shares the AIGatewayRoute name, like the BTP), `jwt.providers[]` carrying the issuer + `remoteJWKS.uri`, and `claimToHeaders: [{claim: teamClaim, header: headerKey}]`. The JWT filter runs before the AI extproc, so an invalid token is a 401 before any model dispatch. Mirrors the spike's validated `07-security-policy.yaml`.
- **The budget-tamper-proof integration.** The `claimToHeaders` mapping derives the team header from a VERIFIED JWT claim. This is exactly the header 2b's `team`-scoped budgets key on (`x-llmkube-team` by default), so once both land the gateway sets the team identity from the token instead of trusting a client-supplied header. 2d-core closes the loop 2b opened. (2d is independent of 2b's code: it references the same header name; the two compose at the data plane regardless of merge order.)
- **Honest boundary (fail-loud), consistent with 2a/2b.** If `auth.jwt` is set but `issuer`, `jwksURI`, or `teamClaim` is missing, set `GatewayReady=False` (reason `InvalidAuth`) and generate nothing rather than emit a half-configured SecurityPolicy that fails open or rejects everything.
- **Status.** Surface auth-configured / fail-loud reason on the ModelRouter condition.

**Deferred to 2d.2, with reasoning:** per-team model **allowlists** (the spike's `authorization` block: default-Deny + per-claim, per-model Allow rules). Authorization is a separate design (default-deny semantics, claim-to-model mapping, how an empty allowlist behaves, status surfacing) and is meatier than authN. 2d-core ships authentication + the budget unlock first so identity and tamper-proof budgets land immediately; 2d.2 adds access control on top of the same SecurityPolicy. Authentication without authorization is still useful here: it establishes verified identity and correct budget attribution; it just does not yet restrict which teams reach which models.

#### Sub-slice 2d.2 (detailed design): per-team model allowlists

Add a per-team model **allowlist** surface and compile it into the `authorization` block of the SAME `SecurityPolicy` 2d-core generates. authZ on top of authN: 2d-core verifies identity, 2d.2 restricts which verified team reaches which model.

- **API surface (`spec.policy.auth.allowlists[]`).** A sibling of `auth.jwt`, so `auth.jwt` is authentication and `auth.allowlists` is authorization. Each entry is `{team, models[]}`: `team` is a verified `teamClaim` value, `models` is the set of model names that team may reach. `models` empty means that team may reach ALL models (identity-only allow). Matches the spike's `allow-team-a-all` (no model restriction) vs `allow-team-b-qwen-only` (model-restricted).

- **Compilation.** When `allowlists` is non-empty, extend the SecurityPolicy with `authorization: {defaultAction: Deny, rules: [...]}`. One Allow rule per entry: `principal.jwt.provider = jwt.Provider`, `principal.jwt.claims = [{name: jwt.TeamClaim, values: [team]}]`, plus (only when `models` is non-empty) `principal.headers = [{name: x-ai-eg-model, values: models}]`. `x-ai-eg-model` is the resolved model header the AI Gateway extproc sets before RBAC runs (validated in the spike); it is the same value `spec.rules[].match.models` route on.

- **Default-deny only when configured.** The `authorization` block is emitted ONLY when `allowlists` is non-empty. An authN-only router (2d-core, no allowlists) keeps allow-all-authenticated semantics, so adding the field cannot retroactively lock out an existing router. A non-empty allowlist flips to default-Deny: any team not named is rejected (403). This is the secure default once a user opts into authorization.

- **Honest boundary (fail-loud), consistent with 2a/2b/2d-core.** `allowlists` set without `auth.jwt` is `AuthorizationRequiresJWT` (you cannot authorize on an unverified claim); an entry with an empty `team`, or a duplicate `team`, is `InvalidAuthorization`. Any of these sets `GatewayReady=False` and generates NOTHING rather than emit a SecurityPolicy whose default-Deny would silently lock everyone out or whose principals are malformed.

- **Status.** On success, append an allowlist summary to the ready message (`; N team model allowlist(s) enforced`), consistent with the JWT and budget summaries.

**Deferred from 2d.2 (with reasoning):** cross-validating allowlist model names against the union of `spec.rules[].match.models` (catch a typo that silently denies) is a useful guard but couples authZ to rule/`defaultRoute` resolution (header-only rules, catch-alls) and is reversible; it is a follow-up. A validating webhook to reject these fail-loud cases at apply time (rather than status-only) is the same cross-slice follow-up noted for 2a/2d-core.

#### Sub-slice 2c (detailed design): audit log via gateway access logs

Deliver the audit trail as a gateway-scoped access log, NOT a per-ModelRouter compiled resource. This is a deliberate departure from the per-route compilation pattern of 2a/2b/2d, forced by how Envoy Gateway works.

- **Why gateway-scoped, not per-router.** Envoy Gateway access logging is configured ONLY on the `EnvoyProxy` resource (`telemetry.accessLog.settings`, `type: Route|Listener`), which attaches to the gateway via `GatewayClass.parametersRef` or `Gateway.spec.infrastructure.parametersRef`. There is no per-`HTTPRoute` or per-`BackendTrafficPolicy` access-log API. The operator references an EXTERNAL Gateway via `gatewayRef` and does not own the GatewayClass / Gateway / EnvoyProxy, so it cannot compile a per-router resource for audit and must not fight other ModelRouters over the shared proxy.

- **The fields are already on the wire.** The spike's validated access-log field set (`start_time`, `x_request_id`, `team` from `x-team`, `model` from `x-ai-eg-model`, `response_model` / `backend` / `llm_input|output|total_token` from the `io.envoy.ai_gateway` dynamic metadata, `response_code`, `duration`) is emitted with NO per-route change: 2d's `claimToHeaders` lands `x-team`, the extproc lands `x-ai-eg-model`, and 2b's `llmRequestCosts` lands the token metadata. 2c is purely the sink config; the producers already shipped.

- **Deliverable: an opt-in, Helm-managed EnvoyProxy access-log template** in `charts/llmkube` (gated on a `gateway.auditLog.enabled` value, default off). It renders the proven field set to two sinks: a `File` sink to `/dev/stdout` (JSON, local fallback) and an `OpenTelemetry` sink (gRPC to a configurable collector, the Loki path), both values-driven. Because the chart cannot own external gateway infra, the chart ships the EnvoyProxy and documents that the operator references it from their GatewayClass/Gateway `parametersRef` (values let them name/namespace it to match).

- **Security defaults (from the spike).** Header allowlist only (NEVER log `Authorization`); PATH and query string omitted; token fields are null on errors and non-final frames (they exist only at end-of-stream on success via `llmRequestCosts`). Request/response bodies are not loggable via access logs at all.

- **Honest boundary (fail-loud), consistent with 2a/2b/2d.** `spec.policy.auditLog` is a Proxy-mode field (it names the router-proxy container and a file path). In `dataPlane: Gateway` mode it has no per-router meaning, so it is fail-loud (`UnsupportedAuditLogInGatewayMode`): set `GatewayReady=False`, generate NOTHING, and point the operator to the gateway-level config. Generate-nothing is especially defensible for audit: silently ignoring an audit directive a user believes is active is a compliance footgun, so refuse loudly. The field stays fully valid in Proxy mode (the router-proxy path is untouched).

**Deferred from 2c (with reasoning):** an operator-managed singleton EnvoyProxy keyed by `gatewayRef` that merges each ModelRouter's `auditLog` into one shared access-log config. It keeps the per-router field meaningful but makes the operator mutate gateway infra it does not own, with cross-router conflict/ordering risk; rejected for v1 in favor of the honest gateway-scoped artifact.

#### Sub-slice 2e-core (detailed design): header-only classification routing + fail-closed sensitive guard

2e splits along `Policy.Classification.Mode`. The `header-only` mode (the default) is a clean compiler slice; the `detector`/`hybrid` in-proxy PII classifier is greenfield (a new ext_proc, build-vs-integrate, never spiked) and is deferred to its own slice (2e-detector). 2e-core ships header-only.

- **`DataClassification` becomes a header match (header-only mode).** Today `RuleMatch.DataClassification` is fail-loud in Gateway mode (`UnsupportedMatchInGatewayMode`). In `header-only` mode the classification is carried by a request header (`Policy.Classification.HeaderKey`, default `x-llmkube-classification`), so a `dataClassification` match compiles exactly like the existing model/header matches: a header match on that key. The values are OR-semantics (the request matches if it carries ANY listed classification), so each classification cross-products with each model (a match entry per `(model, classification)` pair), with the user `headers` ANDed into every entry. `unsupportedMatchFields` becomes mode-aware: it stops rejecting `dataClassification` when mode is `header-only`, and keeps rejecting it in `detector`/`hybrid` (the classifier is not built yet). `taskComplexity`, `requiredCapabilities`, and `latencySLOMs` stay fail-loud in ALL modes (router-side scoring with no gateway equivalent).

- **Fail-closed sensitive guard (the safety payload).** A rule whose `DataClassification` intersects `Policy.Classification.SensitiveClassifications` (default `["pii", "phi"]`) MUST have `Route.FailClosed == true` AND reference only `Tier: local` backends. Otherwise it is fail-loud (`UnsafeSensitiveRoute`): `GatewayReady=False`, generate NOTHING. The backend tiers are resolved by name from `mr.Spec.Backends`. SCOPE (important, do not overclaim): this guard is **per-declaring-rule**, not a global PII-egress invariant. It only inspects rules that explicitly declare a sensitive `dataClassification`; a model-only rule, a catch-all, or the `defaultRoute` that carries pii-headed traffic to some other backend is not inspected. The reason PII genuinely cannot reach a cloud tier *today* is the stronger, separate fact that Gateway mode cannot express a cloud/external backend at all (`resolveBackends` hard-errors on `External`, and CRD validation rejects `tier: cloud` on `InferenceServiceRef` backends). When Gateway mode gains cloud/external backends, the non-declaring egress paths (`defaultRoute`, model-only rules) need their own handling, and real content-based enforcement is the deferred in-proxy classifier (2e-detector).

- **`FailClosed` routing.** With sensitive rules constrained to local-tier backends and the never-empty ejection invariant from 4b, a fail-closed rule whose backends are all down keeps its (local-only) backendRefs and fails the request rather than spilling, which is exactly the fail-closed contract. No extra route construct is needed beyond the validation guard.

**Deferred from 2e-core (with reasoning):** the `detector`/`hybrid` in-proxy classifier (a body-inspecting ext_proc) is the greenfield 2e-detector slice, needing a build-vs-integrate design (Presidio, an LLM classifier, or regex) with latency/accuracy stakes; it gets its own brainstorm. An apply-time validating webhook for the `UnsafeSensitiveRoute` / unsupported-match cases (status-only fail-loud today) remains the cross-slice follow-up noted for 2a/2d/2d.2.

### 5.3 Backend health bridging (sub-projects 3 + 4, issue #662)

**The problem (measured in the spike).** A Metal backend is an fqdn `Backend` pointing at a selectorless Service's ClusterIP. When the Mac dies abruptly, Envoy still sees the ClusterIP as up: in-flight requests stall for the full per-attempt timeout (60s measured) and passive outlier detection never engages (a blackholed connection produces no consecutive 5xx). Pods behind an InferencePool fail new requests over in 2-4ms; off-cluster Metal has no equivalent because pools are structurally pods-only. Something must actively tell Envoy the tier is down.

The chosen posture is **defense in depth**: an always-on active-probe safety net that catches abrupt death at probe speed, plus an event-driven layer that reacts instantly to health the agent/operator already knows. Three reviewable pieces:

#### Slice 4a (detailed design): active health checks on the BTP

Extend the `BackendTrafficPolicy` the operator already generates (2a, which carries retry + passive outlier `healthCheck`) with an `healthCheck.active` HTTP checker (EG v1.8 schema): `type: HTTP`, `http.path: /health` (the llama.cpp/llama-server health endpoint), `method: GET`, `expectedStatuses: [200]`, a short `interval` (default 2s, tunable), `timeout`, `unhealthyThreshold`, `healthyThreshold`. Envoy then probes every backend cluster directly (bypassing the route filters) and ejects a dead host, including an abruptly-killed Mac behind ClusterIP, without waiting on the request timeout. This is the smallest, most self-contained piece and the one that makes the lab failover demo work; it applies to all gateway-mode backends (pods benefit too). Active probing is independent of the long-generation request timeout (it hits `/health`, not the generation path), so it does not reintroduce the short-timeout conflict #662 calls out. The per-backend health path is `/health` for v1; making it configurable per runtime is a follow-up.

#### Slice 3 (detailed design): metal-agent proactive withdraw/restore

metal-agent already runs a health monitor that maintains `process.Healthy` (transition-logged in `health.go`) and re-asserts its Endpoints registration every 30s in `heartbeatOnce` (the steady-state single writer of the Service + EndpointSlice). Today that loop re-registers every process as `Ready: true` unconditionally. Wire the health signal in: make `heartbeatOnce` health-aware so a process the monitor marked **unhealthy** (agent alive, runtime down) is **withdrawn** rather than re-registered.

Withdrawal is a lighter operation than the existing `UnregisterEndpoint` (which deletes the Service + EndpointSlice; that is full teardown, used on delete / scale-to-zero). Instead, a new `WithdrawEndpoint` keeps the Service and EndpointSlice but flips the endpoint's `Conditions.Ready` to `false` and still refreshes the heartbeat annotation. The effect: kube-proxy drops the address from the Service (traffic stops), the operator's `metalEndpointSnapshot` sees `readyReplicas: 0`, and the still-fresh heartbeat distinguishes "agent alive, runtime unhealthy" (`Ready: false`, fresh heartbeat) from "agent dead" (stale heartbeat, the #663 expiry path). On recovery the next heartbeat re-registers it `Ready: true`.

Doing this in the 30s heartbeat loop keeps a single writer and bounds withdrawal latency to one interval; the fast (~4s) Envoy-level ejection for the same unhealthy backend already comes from Slice 4a's active probe (its `/health` returns non-200 when the runtime is down), so this layer's job is the kube-proxy-level cleanup and the operator-observable `readyReplicas: 0` signal that Slice 4b consumes. Abrupt agent death (the Mac is yanked) is still covered by the #663 heartbeat-staleness expiry feeding Slice 4b, and at probe speed by Slice 4a. **Deferred:** immediate withdrawal on the `health.go` healthy->unhealthy transition (event-speed instead of <=30s), which adds a second registry writer and lock-ordering care; the heartbeat-loop convergence is sufficient given Slice 4a.

#### Slice 4b (detailed design): event-driven route-level ejection

The operator recompiles the `AIGatewayRoute` to exclude backends whose referenced InferenceService is unhealthy, and restores them on recovery, the instant the health signal changes rather than at active-probe interval. It sits on top of 4a's safety net, not instead of it.

- **Health signal.** A backend is healthy iff its referenced `InferenceService.Status.ReadyReplicas > 0`. This is the single aggregate that both prior layers already drive to 0: Slice 3's withdrawal flips the metal endpoint to `Ready: false` (the agent-alive-but-unhealthy case), and the #663 heartbeat-staleness expiry zeroes it (the agent-dead case); pod backends get it from pod readiness. `resolveBackends` already fetches each InferenceService, so the check is read off the object already in hand (`Healthy bool` on `routerBackendResource`).

- **Ejection, never to empty.** A pure transform over the compiled rules drops the backendRefs of unhealthy backends from each rule, so a primary-fallback rule whose primary is down routes to the healthy fallback. CRITICAL invariant: if filtering would leave a rule with ZERO backendRefs (all its backends are unhealthy), keep ALL of them instead. An AIGatewayRoute rule with no backendRefs is invalid, and there is nothing healthy to fail over to anyway, so the route stays valid and 4a's active probe carries it. The Backend/AIServiceBackend objects are still generated for every backend (so an ejected backend is ready to re-add on recovery); only the route's backendRefs are filtered.

- **Watch.** Add an `InferenceService -> ModelRouter` watch to the gateway reconciler: a map function that, for a changed InferenceService, enqueues every `dataPlane: Gateway` ModelRouter in that namespace whose `backends[].inferenceServiceRef` names it. A readiness flip thus triggers regeneration within a reconcile, not a probe interval. Reconcile is idempotent (CreateOrUpdate no-ops when the route is unchanged), so the watch firing on unrelated InferenceService updates is harmless.

- **Status.** The ready message reports ejected backends (`; ejected N unhealthy backend(s)`), so an operator sees the route is degraded-but-serving.

- **Debounce.** v1 inherits the hysteresis already in the health signal: the metal death case is debounced by the 3-minute heartbeat timeout (#663), and the withdrawal/pod-readiness cases are deliberate, not flappy. Because 4a's active probe is the always-on safety net, even an un-damped route rewrite cannot cause an outage (it only reconfigures Envoy, which is cheap). An explicit per-backend soak timer (require a state to hold for N seconds before eject/restore) is the **deferred** follow-up if lab observation shows flapping.

**Ordering:** 4a first (self-contained, delivers the demo), then Slice 3 (agent-side, independent), then 4b (the most operator surface: new watch + ejection logic). **Deferred:** explicit flap-soak timer (4b inherits the signal's hysteresis for v1); per-runtime configurable health path (4a hardcodes `/health`); making the active-check thresholds a ModelRouter API surface (hardcoded sensible defaults for v1, mirroring how the passive `healthCheck` block is hardcoded).

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

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/router"
)

// routerProxyConfigKey is the file name inside the controller-managed
// ConfigMap. The router-proxy mounts the ConfigMap at
// routerProxyConfigMountPath and reads <mount>/config.json.
const routerProxyConfigKey = "config.json"

// Built-in URL defaults for first-party providers. Used by
// resolveExternalURL when External.URL is empty.
const (
	defaultAnthropicURL = "https://api.anthropic.com"
	defaultOpenAIURL    = "https://api.openai.com"
)

// compiledConfig captures everything the controller needs after
// translating a ModelRouter spec into the proxy wire shape: the raw
// JSON bytes, a content hash that drives pod rollout, and per-backend
// status the reconciler propagates into Status.Backends.
type compiledConfig struct {
	JSON     []byte
	Hash     string
	Backends []inferencev1alpha1.BackendStatus
	Warnings []string

	// HasPools is true when at least one backend resolved to a ModelPool
	// member. It drives whether the reconciler provisions activation RBAC and
	// runs the proxy with --enable-activation.
	HasPools bool

	// KeyedBackends holds the sorted names of backends whose InferenceService
	// requires an API key the proxy does not forward (see keyedBackends).
	KeyedBackends []string

	// KeyedPoolBackends holds the sorted names of backends that are keyed only
	// because a sibling member of their ModelPool requires an API key. Pool
	// siblings are reachable only through the Proxy data plane, which is why
	// this check lives in the compile and not in keyedBackends, which all
	// three data planes share.
	KeyedPoolBackends []string
}

// compileRouterConfig resolves every backend in the ModelRouter spec
// (InferenceService lookups for local backends, secret-key checks for
// external ones), translates the spec into the proxy wire shape, and
// returns the JSON + content hash plus the BackendStatus list the
// reconciler writes back.
//
// Unresolvable backends are reported as Healthy=false with a Message
// rather than failing the whole compile; the proxy treats them as
// unhealthy and skips them at request time. This matches the existing
// model_controller convention of degraded-but-running over fail-stop.
// A transient lookup error that would change the compiled config (the
// balancing metrics profile) fails the compile instead, keeping the
// previous config.
func (r *ModelRouterReconciler) compileRouterConfig(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) (*compiledConfig, error) {
	out := &router.Config{
		DefaultRoute:         mr.Spec.DefaultRoute,
		DefaultRouteStrategy: string(mr.Spec.DefaultRouteStrategy),
		Backends:             make([]router.Backend, 0, len(mr.Spec.Backends)),
		Rules:                make([]router.Rule, 0, len(mr.Spec.Rules)),
	}
	statuses := make([]inferencev1alpha1.BackendStatus, 0, len(mr.Spec.Backends))
	var warnings []string

	keyed, err := keyedBackends(ctx, r.Client, mr)
	if err != nil {
		return nil, err
	}
	var keyedPool []string

	for i := range mr.Spec.Backends {
		b := &mr.Spec.Backends[i]
		wire, status, err := r.resolveBackend(ctx, mr, b)
		if err != nil {
			return nil, err
		}
		if slices.Contains(keyed, b.Name) {
			status.Healthy = false
			status.Message = appendMsg(status.Message, keyedBackendsMessage([]string{b.Name}))
		} else if wire.Pool != nil && b.InferenceServiceRef != nil {
			siblings, serr := keyedPoolMembers(ctx, r.Client, mr.Namespace, b.InferenceServiceRef.Name, wire.Pool.Members)
			if serr != nil {
				return nil, serr
			}
			if len(siblings) > 0 {
				keyedPool = append(keyedPool, b.Name)
				status.Healthy = false
				status.Message = appendMsg(status.Message, keyedPoolMembersMessage(wire.Pool.Name, siblings))
			}
		}
		if status.Message != "" {
			warnings = append(warnings, fmt.Sprintf("backend %q: %s", b.Name, status.Message))
		}
		out.Backends = append(out.Backends, wire)
		statuses = append(statuses, status)
	}
	slices.Sort(keyedPool)
	hasPools := false
	for i := range out.Backends {
		if out.Backends[i].Pool != nil {
			hasPools = true
			break
		}
	}

	for i := range mr.Spec.Rules {
		out.Rules = append(out.Rules, translateRule(&mr.Spec.Rules[i]))
	}
	out.Policy = translatePolicy(mr.Spec.Policy)

	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("compiled router config failed validation: %w", err)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal router config: %w", err)
	}
	sum := sha256.Sum256(data)

	return &compiledConfig{
		JSON:              data,
		Hash:              hex.EncodeToString(sum[:]),
		Backends:          statuses,
		Warnings:          warnings,
		HasPools:          hasPools,
		KeyedBackends:     keyed,
		KeyedPoolBackends: keyedPool,
	}, nil
}

// resolveBackend turns one ModelRouter backend into its proxy wire
// representation plus the BackendStatus the reconciler reports. An error
// means the backend could not be compiled faithfully and the compile must
// fail, keeping the previous config.
func (r *ModelRouterReconciler) resolveBackend(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	b *inferencev1alpha1.RouterBackend,
) (router.Backend, inferencev1alpha1.BackendStatus, error) {
	now := metav1.Now()
	status := inferencev1alpha1.BackendStatus{
		Name:          b.Name,
		Tier:          b.Tier,
		LastProbeTime: &now,
	}
	wire := router.Backend{
		Name:         b.Name,
		DisplayName:  b.DisplayName,
		Tier:         b.Tier,
		Capabilities: append([]string(nil), b.Capabilities...),
	}
	// Only a set weight is written: absent means the default share of 1 on
	// both sides, and an explicit 0 must survive to drain the backend.
	if b.Weight != nil {
		w := *b.Weight
		wire.Weight = &w
	}
	if b.Timeout != nil {
		wire.Timeout = b.Timeout.Duration
	}
	wire.CostPerMillionTokens = translateTokenCost(b.CostPerMillionTokens)
	var isvc *inferencev1alpha1.InferenceService
	if b.InferenceServiceRef != nil {
		obj := &inferencev1alpha1.InferenceService{}
		err := r.Get(ctx, types.NamespacedName{Name: b.InferenceServiceRef.Name, Namespace: mr.Namespace}, obj)
		switch {
		case err == nil:
			isvc = obj
		case !errors.IsNotFound(err) && b.ModelInfo != nil:
			// Only modelInfo fails the compile on a lookup error, as
			// compileBalancing does; without it the backend compiles as before.
			return wire, status, fmt.Errorf("get InferenceService %s for backend %q modelInfo: %w", b.InferenceServiceRef.Name, b.Name, err)
		}
	}
	info, err := r.compileModelInfo(ctx, mr, b, isvc)
	if err != nil {
		return wire, status, err
	}
	wire.ModelInfo = info

	switch {
	case b.InferenceServiceRef != nil:
		wire.InferenceService = b.InferenceServiceRef.Name
		wire.ServedModel = r.localBackendServedModel(ctx, mr.Namespace, isvc)
		if wire.Tier == "" {
			wire.Tier = backendTierLocal
			status.Tier = backendTierLocal
		}
		if b.Resolution == inferencev1alpha1.RouterBackendResolutionEndpoint {
			endpoints, source, svcAddr, msg := r.resolveInferenceServiceEndpoints(ctx, mr.Namespace, b.InferenceServiceRef.Name)
			wire.Address = svcAddr
			// The proxy tracks the pods itself; the controller compiles only
			// the Service identity, so pod churn does not change the config
			// hash and never re-rolls the proxy. endpoints feeds status only.
			wire.EndpointSource = source
			var err error
			wire.Balancing, wire.Metrics, err = r.compileBalancing(ctx, mr.Namespace, b.InferenceServiceRef.Name, b.Balancing)
			if err != nil {
				return wire, status, err
			}
			r.warnSessionAffinityIgnored(mr, b)
			r.warnPrefixAffinityIgnored(mr, b)
			status.Address = svcAddr
			if len(endpoints) == 0 {
				status.Healthy = false
				status.Message = appendMsg(msg, "no ready endpoints; dispatch falls back to the Service DNS name")
			} else {
				status.Healthy = true
				status.Message = msg
			}
		} else {
			addr, msg := r.resolveInferenceServiceAddress(ctx, mr.Namespace, b.InferenceServiceRef.Name)
			wire.Address = addr
			status.Address = addr
			status.Healthy = addr != ""
			status.Message = msg
		}
		if pool, perr := r.resolveBackendPool(ctx, mr.Namespace, b.InferenceServiceRef.Name); perr != nil {
			// A pool lookup failure must not fail the whole compile; the
			// backend just dispatches without activation (as if unpooled).
			status.Message = appendMsg(status.Message, perr.Error())
		} else if pool != nil {
			wire.Pool = pool
		}
	case b.External != nil:
		if wire.Tier == "" {
			wire.Tier = backendTierCloud
			status.Tier = backendTierCloud
		}
		wire.Provider = b.External.Provider
		wire.Model = b.External.Model
		addr, urlMsg := r.resolveExternalURL(b.External)
		wire.Address = addr
		status.Address = addr
		if addr == "" {
			status.Healthy = false
			status.Message = urlMsg
			return wire, status, nil
		}
		if b.External.CredentialsSecretRef != nil {
			// Resolving the well-known env var only when the user
			// declared a Secret keeps backends like LiteLLM (which
			// proxy auth themselves) and in-cluster sidecars (a
			// non-LLMKube vLLM, an OpenAI-shape mock) able to opt out
			// of credential injection. Without this gate the proxy
			// would refuse to dispatch with "credentials env X is
			// unset" even though the backend never needed auth.
			wire.CredentialsEnv = wellKnownCredEnv(b.External.Provider)
			if err := r.assertCredentialsSecretExists(ctx, mr.Namespace,
				b.External.CredentialsSecretRef.Name, wire.CredentialsEnv); err != nil {
				status.Healthy = false
				status.Message = err.Error()
			} else {
				status.Healthy = true
			}
		} else {
			status.Healthy = true
		}
	default:
		status.Healthy = false
		status.Message = "no inferenceServiceRef or external provider declared"
	}
	return wire, status, nil
}

// resolveExternalURL applies the per-provider URL default when the user
// did not specify one. First-party providers have a single published
// endpoint we can hardcode; LiteLLM has no universal default but
// operators can configure a cluster-wide one via --default-litellm-url.
// Returns ("", msg) when no URL is available so the caller can surface
// the misconfiguration in BackendStatus.Message instead of writing an
// empty Address into the compiled config (which the proxy would later
// reject at dispatch time with a less actionable error).
func (r *ModelRouterReconciler) resolveExternalURL(p *inferencev1alpha1.ExternalProvider) (string, string) {
	if p.URL != "" {
		return p.URL, ""
	}
	switch p.Provider {
	case "anthropic":
		return defaultAnthropicURL, ""
	case "openai":
		return defaultOpenAIURL, ""
	case "litellm":
		if r.DefaultLiteLLMURL != "" {
			return r.DefaultLiteLLMURL, ""
		}
		return "", "external backend with provider=litellm requires url " +
			"(or operator-configured --default-litellm-url)"
	default:
		return "", fmt.Sprintf("external backend with provider=%q requires url "+
			"(no built-in default for this provider)", p.Provider)
	}
}

// resolveInferenceServiceAddress builds the cluster URL the router-proxy
// will POST to for a local backend. Returns ("", message) when the
// InferenceService doesn't exist; the caller surfaces that on
// Status.Backends.
func (r *ModelRouterReconciler) resolveInferenceServiceAddress(
	ctx context.Context,
	namespace, name string,
) (string, string) {
	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, isvc); err != nil {
		if errors.IsNotFound(err) {
			return "", fmt.Sprintf("InferenceService %q not found in namespace %q", name, namespace)
		}
		return "", fmt.Sprintf("InferenceService %q lookup failed: %v", name, err)
	}
	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}
	svcName := sanitizeDNSName(isvc.Name)
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svcName, isvc.Namespace, port), ""
}

// localBackendServedModel returns the model identifier the runtime behind an
// InferenceService actually serves, and "" to keep the InferenceService name.
// vLLM and SGLang reject a request for a name they do not serve, so they get
// the name their container is started with (servedModelNames): the user's
// --served-model-name, else the modelRef (#1988). oMLX serves the model-store
// directory basename, mirroring the agent's model-id derivation in
// pkg/agent/executor_omlx.go (filepath.Base(ModelSource)); the two sides must
// agree or the router rewrites to a name oMLX rejects. llama.cpp ignores the
// field. A missing InferenceService or Model, or an empty ModelRef, degrades
// to "" (the InferenceService name fallback) rather than failing the compile;
// the address path already reports the backend unhealthy.
func (r *ModelRouterReconciler) localBackendServedModel(
	ctx context.Context,
	namespace string,
	isvc *inferencev1alpha1.InferenceService,
) string {
	if isvc == nil {
		return ""
	}
	switch runtimeNameLabel(isvc) {
	case RuntimeVLLM, RuntimeSGLANG:
		if names := servedModelNames(isvc); len(names) > 0 {
			return names[0]
		}
		return ""
	case inferencev1alpha1.RuntimeOMLX:
	default:
		return ""
	}
	if isvc.Spec.ModelRef == "" {
		return ""
	}
	model := &inferencev1alpha1.Model{}
	if err := r.Get(ctx, types.NamespacedName{Name: isvc.Spec.ModelRef, Namespace: namespace}, model); err != nil {
		return ""
	}
	if model.Spec.Source != "" {
		return filepath.Base(model.Spec.Source)
	}
	return model.Name
}

// resolveInferenceServiceEndpoints returns the ready pod URLs (used for status
// only), the Service identity the proxy watches, and the Service DNS URL to
// fall back to. Endpoint resolution is opt-in
// (spec.backends[].resolution = "endpoint") because it only helps an
// in-cluster backend whose pod churn the controller can watch; the external
// path stays service-level, and a metal member is out-of-cluster and has no
// EndpointSlices at all.
func (r *ModelRouterReconciler) resolveInferenceServiceEndpoints(
	ctx context.Context,
	namespace, name string,
) (endpoints []string, source *router.EndpointSource, serviceAddr, msg string) {
	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, isvc); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, "", fmt.Sprintf("InferenceService %q not found in namespace %q", name, namespace)
		}
		return nil, nil, "", fmt.Sprintf("InferenceService %q lookup failed: %v", name, err)
	}
	port := int32(8080)
	if isvc.Spec.Endpoint != nil && isvc.Spec.Endpoint.Port > 0 {
		port = isvc.Spec.Endpoint.Port
	}
	svcName := sanitizeDNSName(isvc.Name)
	serviceAddr = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", svcName, isvc.Namespace, port)
	source = &router.EndpointSource{Namespace: isvc.Namespace, Service: svcName, Port: port}

	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, endpointSlices,
		client.InNamespace(namespace),
		client.MatchingLabels{"kubernetes.io/service-name": svcName}); err != nil {
		return nil, source, serviceAddr, fmt.Sprintf("list EndpointSlices for %q: %v", svcName, err)
	}
	return collectReadyReplicaURLs(endpointSlices, port), source, serviceAddr, ""
}

// resolveBackendPool finds the ModelPool a member InferenceService belongs to
// (if any) and compiles the BackendPool the proxy needs to activate it. Returns
// (nil, nil) when the member is not pooled. Membership is a mapfunc-style scan
// over ModelPools in the namespace; a member can belong to at most one pool
// (the first match wins, matching the exclusive-slot invariant).
func (r *ModelRouterReconciler) resolveBackendPool(
	ctx context.Context,
	namespace, member string,
) (*router.BackendPool, error) {
	pools := &inferencev1alpha1.ModelPoolList{}
	if err := r.List(ctx, pools, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list ModelPools: %w", err)
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		members := make([]string, 0, len(pool.Spec.Members))
		isMember := false
		for _, m := range pool.Spec.Members {
			members = append(members, m.InferenceServiceRef.Name)
			if m.InferenceServiceRef.Name == member {
				isMember = true
			}
		}
		if !isMember {
			continue
		}
		bp := &router.BackendPool{
			Name:      pool.Name,
			Namespace: pool.Namespace,
			Member:    member,
			Members:   members,
		}
		if pool.Spec.SwapBudget != nil {
			bp.SwapBudget = pool.Spec.SwapBudget.Duration
		}
		return bp, nil
	}
	return nil, nil
}

// appendMsg joins two status messages, tolerating an empty base.
func appendMsg(base, extra string) string {
	if base == "" {
		return extra
	}
	if extra == "" {
		return base
	}
	return base + "; " + extra
}

// assertCredentialsSecretExists verifies the referenced Secret is present
// and contains the expected key. Catches the misconfigured-secret case
// at reconcile time rather than at first request.
func (r *ModelRouterReconciler) assertCredentialsSecretExists(
	ctx context.Context,
	namespace, name, expectedKey string,
) error {
	sec := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sec); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("credentials Secret %q not found", name)
		}
		return fmt.Errorf("credentials Secret %q lookup failed: %w", name, err)
	}
	if expectedKey == "" {
		return nil
	}
	if _, ok := sec.Data[expectedKey]; !ok {
		return fmt.Errorf("credentials Secret %q missing key %q", name, expectedKey)
	}
	return nil
}

// wellKnownCredEnv maps a provider name to the conventional environment
// variable the router-proxy expects to read. Mirrors the proxy's
// applyCredentials switch in internal/router/backend.go.
func wellKnownCredEnv(provider string) string {
	switch provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "litellm":
		return "LITELLM_MASTER_KEY"
	case "bedrock":
		return "AWS_ACCESS_KEY_ID"
	case "vertex_ai":
		return "GOOGLE_APPLICATION_CREDENTIALS"
	default:
		return ""
	}
}

// translateRule maps a ModelRouter spec rule to the proxy wire shape.
// Pure transformation; no I/O.
func translateRule(in *inferencev1alpha1.RouterRule) router.Rule {
	out := router.Rule{
		Name:       in.Name,
		FailClosed: in.FailClosed,
		Route: router.RuleRoute{
			Backends:       append([]string(nil), in.Route.Backends...),
			Strategy:       in.Route.Strategy,
			PoolActivation: in.Route.PoolActivation,
		},
	}
	if in.Match != nil {
		out.Match = router.RuleMatch{
			DataClassification:   append([]string(nil), in.Match.DataClassification...),
			TaskComplexity:       in.Match.TaskComplexity,
			RequiredCapabilities: append([]string(nil), in.Match.RequiredCapabilities...),
			Headers:              copyStringMap(in.Match.Headers),
			Models:               append([]string(nil), in.Match.Models...),
		}
	}
	if in.Timeout != nil {
		out.Timeout = in.Timeout.Duration
	}
	return out
}

// translatePolicy maps the ModelRouter policy block to the proxy shape.
// A nil input policy yields a defaulted-but-valid wire policy.
func translatePolicy(p *inferencev1alpha1.RouterPolicy) router.Policy {
	out := router.Policy{
		Classification: router.ClassificationPolicy{Mode: "header-only"},
		AuditLog:       router.AuditLogPolicy{Sink: "stdout"},
	}
	if p == nil {
		return out
	}
	if p.Classification != nil {
		out.Classification = router.ClassificationPolicy{
			Mode:      p.Classification.Mode,
			HeaderKey: p.Classification.HeaderKey,
			Sensitive: append([]string(nil), p.Classification.SensitiveClassifications...),
		}
		if out.Classification.Mode == "" {
			out.Classification.Mode = "header-only"
		}
	}
	if p.AuditLog != nil {
		out.AuditLog = router.AuditLogPolicy{
			Sink:               p.AuditLog.Sink,
			FilePath:           p.AuditLog.FilePath,
			IncludeRequestBody: p.AuditLog.IncludeRequestBody,
			LogClientAddress:   p.AuditLog.LogClientAddress,
			TrustedProxyCIDRs:  append([]string(nil), p.AuditLog.TrustedProxyCIDRs...),
			CallerHeader:       p.AuditLog.CallerHeader,
		}
		if out.AuditLog.Sink == "" {
			out.AuditLog.Sink = "stdout"
		}
	}
	out.Budgets = translateBudgets(p.Budgets)
	return out
}

// translateBudgets maps the CRD's budget list into the proxy's rule shape.
// A team budget keeps its header key: the proxy resolves concrete team
// values from that header at request time, because the controller cannot
// know them in advance. The window defaults to one hour to match the CRD
// default. Malformed MaxUSD contributes zero; controller validation already
// rejects malformed values.
func translateBudgets(in []inferencev1alpha1.BudgetSpec) []router.Budget {
	if len(in) == 0 {
		return nil
	}
	out := make([]router.Budget, 0, len(in))
	for _, b := range in {
		window := time.Duration(b.WindowSeconds) * time.Second
		if window <= 0 {
			window = time.Hour
		}
		var tokens int64
		if b.MaxTokens != nil {
			tokens = *b.MaxTokens
		}
		out = append(out, router.Budget{
			Name:      b.Name,
			Scope:     b.Scope,
			RuleName:  b.RuleName,
			HeaderKey: b.HeaderKey,
			Window:    window,
			MaxTokens: tokens,
			MaxUSD:    parseUSD(b.MaxUSD),
		})
	}
	return out
}

// translateTokenCost converts the CRD's decimal-string pricing into the
// proxy's float form. A nil input yields nil so backends without declared
// pricing cost 0 USD against dollar budgets.
func translateTokenCost(c *inferencev1alpha1.TokenCost) *router.TokenCost {
	if c == nil {
		return nil
	}
	return &router.TokenCost{
		PromptUSD:     parseUSD(c.PromptUSD),
		CompletionUSD: parseUSD(c.CompletionUSD),
	}
}

// modelRouterReasonModelInfoMismatch is the event reason for a backend whose
// capabilities list vision while its modelInfo.inputModalities leave out image.
const modelRouterReasonModelInfoMismatch = "ModelInfoMismatch"

// compileModelInfo translates a backend's modelInfo for /v1/models (#2248).
// Only a backend that declares modelInfo publishes anything, so a router
// without it keeps its config hash and its proxy does not roll. Fields left
// unset come from what the controller already knows: a "vision" capability
// means text and image input, other modalities default to text, and the
// context length comes from the InferenceService (nil when the backend has
// none or it is not found). A lookup error other than NotFound is returned,
// as in compileBalancing, so a transient failure cannot change the hash twice.
func (r *ModelRouterReconciler) compileModelInfo(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	b *inferencev1alpha1.RouterBackend,
	isvc *inferencev1alpha1.InferenceService,
) (*router.ModelInfo, error) {
	m := b.ModelInfo
	if m == nil {
		return nil, nil
	}
	out := &router.ModelInfo{
		InputModalities:  append([]string(nil), m.InputModalities...),
		OutputModalities: append([]string(nil), m.OutputModalities...),
	}
	if m.ContextLength != nil {
		out.ContextLength = *m.ContextLength
	}
	if slices.Contains(b.Capabilities, backendCapabilityVision) {
		if len(out.InputModalities) == 0 {
			out.InputModalities = []string{"text", "image"}
		} else if !slices.Contains(out.InputModalities, "image") && r.Recorder != nil {
			r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, modelRouterReasonModelInfoMismatch, "Reconcile",
				"backend %q lists the vision capability but modelInfo.inputModalities leaves out image; /v1/models publishes modelInfo.inputModalities",
				b.Name)
		}
	}
	if len(out.InputModalities) == 0 {
		out.InputModalities = []string{"text"}
	}
	if len(out.OutputModalities) == 0 {
		out.OutputModalities = []string{"text"}
	}
	if out.ContextLength == 0 && isvc != nil {
		agent, err := r.runtimeSetByMetalAgent(ctx, isvc)
		if err != nil {
			return nil, fmt.Errorf("backend %q modelInfo: %w", b.Name, err)
		}
		if !agent {
			out.ContextLength = servedContextLength(isvc)
		}
	}
	return out, nil
}

// runtimeSetByMetalAgent reports whether an unset runtime is left to the
// metal-agent's --runtime flag, which may not be llama.cpp. A missing Model
// also reports true, so its context length is not guessed.
func (r *ModelRouterReconciler) runtimeSetByMetalAgent(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
) (bool, error) {
	if isvc.Spec.Runtime != "" {
		return false, nil
	}
	if isvc.Spec.ModelRef == "" {
		return true, nil
	}
	model := &inferencev1alpha1.Model{}
	err := r.Get(ctx, types.NamespacedName{Name: isvc.Spec.ModelRef, Namespace: isvc.Namespace}, model)
	switch {
	case err == nil:
		return isMetalModel(model), nil
	case errors.IsNotFound(err):
		return true, nil
	default:
		return false, fmt.Errorf("get Model %s: %w", isvc.Spec.ModelRef, err)
	}
}

// backendCapabilityVision is the RouterBackend capability that marks a
// backend as taking image input.
const backendCapabilityVision = "vision"

// servedContextLength is the context window one request gets, read from the
// spec fields the controller renders onto the runtime's command line. It is
// zero when that can't be known: a runtime without a known flag, or extraArgs
// that set the flags themselves.
func servedContextLength(isvc *inferencev1alpha1.InferenceService) int64 {
	args := isvc.Spec.ExtraArgs
	switch isvc.Spec.Runtime {
	case RuntimeVLLM:
		// extraArgs follow the rendered flags, so vLLM keeps their value.
		if values, ok := extraArgValues(args, "max-model-len"); ok {
			if len(values) == 0 {
				return 0
			}
			n, err := strconv.ParseInt(values[len(values)-1], 10, 64)
			if err != nil || n < 1 {
				return 0
			}
			return n
		}
		if cfg := isvc.Spec.VLLMConfig; cfg != nil && cfg.MaxModelLen != nil {
			return int64(*cfg.MaxModelLen)
		}
	case "", "llamacpp":
		if hasMatchingExtraArgAny(args, "ctx-size", "-c", "parallel", "-np",
			"kv-unified", "-kvu", "no-kv-unified", "-no-kvu", "kv-unified-per-slot") {
			return 0
		}
		if isvc.Spec.ContextSize == nil {
			return 0
		}
		// llama-server keeps one shared KV pool when --parallel is left on
		// auto, but an explicit --parallel splits --ctx-size evenly across
		// the slots, each rounded up to 256 tokens (llama.cpp b11854,
		// llama_context: n_ctx_seq = n_ctx / n_seq_max).
		n := padTo256(int64(*isvc.Spec.ContextSize))
		if p := isvc.Spec.ParallelSlots; p != nil && *p > 1 {
			n = padTo256(n / int64(*p))
		}
		return n
	}
	// llamacpp-router loads models on demand, each with its own context, so
	// there is no single length to publish.
	return 0
}

func padTo256(n int64) int64 {
	return (n + 255) / 256 * 256
}

// parseUSD parses a validated non-negative decimal string. Malformed input
// (which the CRD pattern rejects) contributes zero rather than failing the
// whole compile.
func parseUSD(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// reconcileRouterConfigMap creates or updates the ConfigMap that backs
// the router-proxy. The ConfigMap is owner-referenced to the ModelRouter
// so deleting the parent garbage-collects the config.
func (r *ModelRouterReconciler) reconcileRouterConfigMap(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	compiled *compiledConfig,
) error {
	desired := newRouterConfigMap(mr, compiled)
	if err := setControllerReferenceUnblocked(mr, desired, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on router ConfigMap: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	switch {
	case errors.IsNotFound(err):
		return r.Create(ctx, desired)
	case err != nil:
		return err
	}
	// Before the no-op fast path below: a foreign object that happens to hold
	// identical data must still block the sequence, not silently succeed and
	// let the Deployment provision against it.
	if err := ensureRouterProxyOwned(mr, existing, "ConfigMap"); err != nil {
		return err
	}

	if existing.Data[routerProxyConfigKey] == desired.Data[routerProxyConfigKey] &&
		existing.Annotations[routerProxyConfigHashAnnotation] == compiled.Hash {
		return nil
	}
	existing.Data = desired.Data
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[routerProxyConfigHashAnnotation] = compiled.Hash
	existing.Labels = desired.Labels
	if err := setControllerReferenceUnblocked(mr, existing, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on existing router ConfigMap: %w", err)
	}
	return r.Update(ctx, existing)
}

// newRouterConfigMap is the in-memory blueprint of the ConfigMap. Kept
// separate from reconcileRouterConfigMap so unit tests can call it
// without a fake client.
func newRouterConfigMap(mr *inferencev1alpha1.ModelRouter, compiled *compiledConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerProxyResourceName(mr.Name),
			Namespace: mr.Namespace,
			Labels:    routerProxyLabels(mr),
			Annotations: map[string]string{
				routerProxyConfigHashAnnotation: compiled.Hash,
			},
		},
		Data: map[string]string{
			routerProxyConfigKey: string(compiled.JSON),
		},
	}
}

// vllmMetricsProfile is the engine metrics the router proxy reads from a vLLM
// pod for LoadAware placement. Names as served by vLLM's /metrics on the API
// port (vllm/v1/metrics/loggers.py).
func vllmMetricsProfile() *router.MetricsProfile {
	return &router.MetricsProfile{
		Path:    "/metrics",
		Running: "vllm:num_requests_running",
		Waiting: "vllm:num_requests_waiting",
		KVUsage: "vllm:kv_cache_usage_perc",
	}
}

// modelRouterReasonSessionAffinityIgnored is the event reason for a backend
// that sets sessionAffinity without mode: LoadAware.
const modelRouterReasonSessionAffinityIgnored = "SessionAffinityIgnored"

// warnSessionAffinityIgnored emits one Warning event when a backend carries
// sessionAffinity but is not LoadAware, so the compiled config silently
// dropping the headers is visible to the operator. Nil-safe on the recorder.
func (r *ModelRouterReconciler) warnSessionAffinityIgnored(mr *inferencev1alpha1.ModelRouter, b *inferencev1alpha1.RouterBackend) {
	if r.Recorder == nil || b.Balancing == nil || b.Balancing.SessionAffinity == nil ||
		b.Balancing.Mode == inferencev1alpha1.BalancingModeLoadAware {
		return
	}
	r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, modelRouterReasonSessionAffinityIgnored, "Reconcile",
		"backend %q sets balancing.sessionAffinity but balancing.mode is not LoadAware, so session affinity is ignored; set balancing.mode: LoadAware to enable it",
		b.Name)
}

// modelRouterReasonPrefixAffinityIgnored is the event reason for a backend
// that sets prefixAffinity without mode: LoadAware.
const modelRouterReasonPrefixAffinityIgnored = "PrefixAffinityIgnored"

// warnPrefixAffinityIgnored is warnSessionAffinityIgnored for prefixAffinity.
func (r *ModelRouterReconciler) warnPrefixAffinityIgnored(mr *inferencev1alpha1.ModelRouter, b *inferencev1alpha1.RouterBackend) {
	if r.Recorder == nil || b.Balancing == nil || b.Balancing.PrefixAffinity == nil ||
		b.Balancing.Mode == inferencev1alpha1.BalancingModeLoadAware {
		return
	}
	r.Recorder.Eventf(mr, nil, corev1.EventTypeWarning, modelRouterReasonPrefixAffinityIgnored, "Reconcile",
		"backend %q sets balancing.prefixAffinity but balancing.mode is not LoadAware, so prefix affinity is ignored; set balancing.mode: LoadAware to enable it",
		b.Name)
}

// compileBalancing translates a backend's balancing block, and attaches the
// metrics profile for runtimes LLMKube has one for. A missing
// InferenceService compiles without a profile; any other lookup error is
// returned, because compiling without the profile would change the config
// hash and roll the proxy twice (once now, once when the lookup recovers).
func (r *ModelRouterReconciler) compileBalancing(ctx context.Context, namespace, isvcName string, b *inferencev1alpha1.BackendBalancing) (*router.Balancing, *router.MetricsProfile, error) {
	if b == nil {
		return nil, nil, nil
	}
	out := &router.Balancing{Mode: b.Mode}
	// Session affinity only means something to LoadAware placement. Under
	// RoundRobin the headers are not compiled; resolveBackend warns about it.
	if sa := b.SessionAffinity; sa != nil && out.Mode == router.BalancingLoadAware {
		out.SessionHeaders = append([]string(nil), sa.Headers...)
		if sa.IdleTimeout != nil {
			out.SessionIdleTimeout = sa.IdleTimeout.Duration
		}
	}
	// Likewise prefix affinity; resolveBackend warns about it under RoundRobin.
	// An object stored before the CRD defaulted blockBytes compiles with the
	// default rather than off; maxBlocksPerEndpoint 0 is the proxy's default.
	if pa := b.PrefixAffinity; pa != nil && out.Mode == router.BalancingLoadAware {
		out.PrefixBlockBytes = int(pa.BlockBytes)
		if out.PrefixBlockBytes == 0 {
			out.PrefixBlockBytes = router.DefaultPrefixBlockBytes
		}
		out.PrefixMaxBlocks = int(pa.MaxBlocksPerEndpoint)
	}
	if out.Mode != router.BalancingLoadAware {
		return out, nil, nil
	}
	isvc := &inferencev1alpha1.InferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: namespace}, isvc); err != nil {
		if errors.IsNotFound(err) {
			return out, nil, nil
		}
		return nil, nil, fmt.Errorf("backend balancing: get InferenceService %s/%s: %w", namespace, isvcName, err)
	}
	if resolveServingRuntime(isvc) == RuntimeVLLM {
		return out, vllmMetricsProfile(), nil
	}
	return out, nil, nil
}

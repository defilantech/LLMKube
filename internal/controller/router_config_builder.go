/*
Copyright 2025.

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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

	for i := range mr.Spec.Backends {
		b := &mr.Spec.Backends[i]
		wire, status := r.resolveBackend(ctx, mr, b)
		if status.Message != "" {
			warnings = append(warnings, fmt.Sprintf("backend %q: %s", b.Name, status.Message))
		}
		out.Backends = append(out.Backends, wire)
		statuses = append(statuses, status)
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
		JSON:     data,
		Hash:     hex.EncodeToString(sum[:]),
		Backends: statuses,
		Warnings: warnings,
	}, nil
}

// resolveBackend turns one ModelRouter backend into its proxy wire
// representation plus the BackendStatus the reconciler reports.
func (r *ModelRouterReconciler) resolveBackend(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
	b *inferencev1alpha1.RouterBackend,
) (router.Backend, inferencev1alpha1.BackendStatus) {
	now := metav1.Now()
	status := inferencev1alpha1.BackendStatus{
		Name:          b.Name,
		Tier:          b.Tier,
		LastProbeTime: &now,
	}
	wire := router.Backend{
		Name:         b.Name,
		Tier:         b.Tier,
		Capabilities: append([]string(nil), b.Capabilities...),
	}
	if b.Weight != nil {
		wire.Weight = int(*b.Weight)
	}
	if b.Timeout != nil {
		wire.Timeout = b.Timeout.Duration
	}

	switch {
	case b.InferenceServiceRef != nil:
		if wire.Tier == "" {
			wire.Tier = backendTierLocal
			status.Tier = backendTierLocal
		}
		addr, msg := r.resolveInferenceServiceAddress(ctx, mr.Namespace, b.InferenceServiceRef.Name)
		wire.Address = addr
		status.Address = addr
		status.Healthy = addr != ""
		status.Message = msg
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
			return wire, status
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
	return wire, status
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
			Backends: append([]string(nil), in.Route.Backends...),
			Strategy: in.Route.Strategy,
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
		}
		if out.AuditLog.Sink == "" {
			out.AuditLog.Sink = "stdout"
		}
	}
	return out
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

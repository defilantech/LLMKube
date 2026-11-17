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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// routerResolutionError reports a backend resolution that failed on cluster
// state rather than on the spec, so the reconciler records it as a Validated
// and Available failure without treating the spec as malformed.
type routerResolutionError struct {
	errs []ModelRouterValidationError
}

func (e *routerResolutionError) Error() string {
	return formatValidationErrors(e.errs)
}

// resolveRouterBackends returns the backends the compile should render: the
// explicit entries in declaration order followed by every InferenceService
// spec.backendSelector selects and no explicit entry claims, sorted by name.
// The second result names every backend that came from the selector, whether
// synthesized or an overlay of a selected service, so the compile can tell it
// from an entry the user wrote.
// A router without a selector returns spec.backends untouched and makes no
// API call. The returned validation errors describe cluster state (an
// overlay entry that matches nothing, a selector that selects nothing), not
// the spec.
func (r *ModelRouterReconciler) resolveRouterBackends(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) ([]inferencev1alpha1.RouterBackend, map[string]bool, []ModelRouterValidationError, error) {
	if mr.Spec.BackendSelector == nil {
		return mr.Spec.Backends, nil, nil, nil
	}
	selected, err := r.selectedInferenceServices(ctx, mr)
	if err != nil {
		return nil, nil, nil, err
	}

	var errs []ModelRouterValidationError
	resolved := make([]inferencev1alpha1.RouterBackend, 0, len(mr.Spec.Backends)+len(selected))
	selectorSourced := make(map[string]bool, len(selected))
	claimed := make(map[string]bool, len(mr.Spec.Backends))
	for i := range mr.Spec.Backends {
		entry := &mr.Spec.Backends[i]
		if entry.InferenceServiceRef != nil || entry.External != nil {
			resolved = append(resolved, *entry.DeepCopy())
			claimed[entry.Name] = true
			continue
		}
		isvc, ok := selected[entry.Name]
		if !ok {
			errs = append(errs, ModelRouterValidationError{
				Field: fmt.Sprintf("spec.backends[%d].name", i),
				Message: fmt.Sprintf("backend %q matches no InferenceService selected by spec.backendSelector",
					entry.Name),
			})
			continue
		}
		resolved = append(resolved, overlaySelectedBackend(isvc, entry))
		selectorSourced[entry.Name] = true
		claimed[entry.Name] = true
	}

	unclaimed := make([]string, 0, len(selected))
	for name := range selected {
		if !claimed[name] {
			unclaimed = append(unclaimed, name)
		}
	}
	slices.Sort(unclaimed)
	for _, name := range unclaimed {
		resolved = append(resolved, selectedBackend(selected[name]))
		selectorSourced[name] = true
	}

	if len(resolved) == 0 && len(errs) == 0 {
		errs = append(errs, ModelRouterValidationError{
			Field:   "spec.backendSelector",
			Message: "selects no InferenceService, and spec.backends is empty, so the router has no backend",
		})
	}
	return resolved, selectorSourced, errs, nil
}

// selectedInferenceServices lists the InferenceServices in the router's
// namespace that spec.backendSelector matches, keyed by name.
func (r *ModelRouterReconciler) selectedInferenceServices(
	ctx context.Context,
	mr *inferencev1alpha1.ModelRouter,
) (map[string]*inferencev1alpha1.InferenceService, error) {
	selector, err := metav1.LabelSelectorAsSelector(mr.Spec.BackendSelector)
	if err != nil {
		return nil, &routerResolutionError{errs: []ModelRouterValidationError{{
			Field:   "spec.backendSelector",
			Message: err.Error(),
		}}}
	}
	list := &inferencev1alpha1.InferenceServiceList{}
	if err := r.List(ctx, list, client.InNamespace(mr.Namespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("list InferenceServices for spec.backendSelector: %w", err)
	}
	out := make(map[string]*inferencev1alpha1.InferenceService, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}

// selectedBackend is the backend a selected InferenceService compiles to: its
// name verbatim, the reference identity, local tier and Service resolution.
func selectedBackend(isvc *inferencev1alpha1.InferenceService) inferencev1alpha1.RouterBackend {
	return inferencev1alpha1.RouterBackend{
		Name:                isvc.Name,
		InferenceServiceRef: &corev1.LocalObjectReference{Name: isvc.Name},
		Tier:                backendTierLocal,
		Resolution:          inferencev1alpha1.RouterBackendResolutionService,
	}
}

// overlaySelectedBackend starts from the selected backend's identity and
// copies every field the explicit entry sets, so an override carries only what
// the user wrote.
func overlaySelectedBackend(
	isvc *inferencev1alpha1.InferenceService,
	entry *inferencev1alpha1.RouterBackend,
) inferencev1alpha1.RouterBackend {
	out := selectedBackend(isvc)
	if entry.DisplayName != "" {
		out.DisplayName = entry.DisplayName
	}
	if entry.ModelInfo != nil {
		out.ModelInfo = entry.ModelInfo
	}
	if entry.Tier != "" {
		out.Tier = entry.Tier
	}
	if entry.Resolution != "" {
		out.Resolution = entry.Resolution
	}
	if entry.EndpointPicker != nil {
		out.EndpointPicker = entry.EndpointPicker
	}
	if entry.Balancing != nil {
		out.Balancing = entry.Balancing
	}
	if len(entry.Capabilities) > 0 {
		out.Capabilities = entry.Capabilities
	}
	if entry.Weight != nil {
		out.Weight = entry.Weight
	}
	if entry.CostPerMillionTokens != nil {
		out.CostPerMillionTokens = entry.CostPerMillionTokens
	}
	if entry.HealthCheck != nil {
		out.HealthCheck = entry.HealthCheck
	}
	if entry.Timeout != nil {
		out.Timeout = entry.Timeout
	}
	return out
}

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// ModelRouterReasonBackendAPIKeyUnsupported marks a router with a backend
// InferenceService that requires an API key: spec.endpoint.apiKeySecretRef,
// --api-key in spec.extraArgs, --api-key in spec.args with spec.command set,
// or VLLM_API_KEY in spec.env (vLLM). No data plane forwards that key yet, so
// every request to it would get 401.
//
//nolint:gosec // G101: condition reason string, not a credential value
const ModelRouterReasonBackendAPIKeyUnsupported = "BackendAPIKeyUnsupported"

// keyedBackends returns the sorted names of InferenceServiceRef backends whose
// InferenceService requires an API key in any form a user can set it. The
// verdict must come from inferenceServiceRequiresAPIKey, the same helper the
// LoRAAdapter load refusal and unload gate use, so all consumers agree on
// what keyed means. A backend whose InferenceService does not exist is not
// keyed; the resolver reports it.
func keyedBackends(
	ctx context.Context,
	c client.Reader,
	namespace string,
	backends []inferencev1alpha1.RouterBackend,
) ([]string, error) {
	var names []string
	for i := range backends {
		b := &backends[i]
		if b.InferenceServiceRef == nil {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: b.InferenceServiceRef.Name}, isvc)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get InferenceService %s/%s: %w", namespace, b.InferenceServiceRef.Name, err)
		}
		if inferenceServiceRequiresAPIKey(isvc) {
			names = append(names, b.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// keyedBackendsMessage names the backends and all three keying forms; tests
// pin the apiKeySecretRef and VLLM_API_KEY substrings.
func keyedBackendsMessage(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf("backend(s) %s point at an InferenceService that requires an API key "+
		"(spec.endpoint.apiKeySecretRef, --api-key, or VLLM_API_KEY in spec.env (vLLM)); ModelRouter does not forward that key yet, "+
		"so requests would get 401. Remove the key "+
		"from the InferenceService or route to it directly", strings.Join(quoted, ", "))
}

// keyedPoolMembers returns the sorted names of pool members other than self
// whose InferenceService requires an API key. The member list comes from the
// BackendPool resolveBackendPool compiled, so the first-match pool selection
// here can never disagree with the wire config. A member whose
// InferenceService is gone is not keyed; the resolver reports it.
func keyedPoolMembers(ctx context.Context, c client.Reader, namespace, self string, members []string) ([]string, error) {
	var keyed []string
	for _, name := range members {
		if name == self {
			continue
		}
		isvc := &inferencev1alpha1.InferenceService{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, isvc)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get InferenceService %s/%s: %w", namespace, name, err)
		}
		if inferenceServiceRequiresAPIKey(isvc) {
			keyed = append(keyed, name)
		}
	}
	slices.Sort(keyed)
	return keyed, nil
}

// keyedPoolMembersMessage names the pool and its keyed members for the
// backend's own status entry.
func keyedPoolMembersMessage(pool string, members []string) string {
	quoted := make([]string, len(members))
	for i, m := range members {
		quoted[i] = fmt.Sprintf("%q", m)
	}
	return fmt.Sprintf("pool %q member(s) %s require an API key; ModelRouter does not forward "+
		"that key yet, so requests to them would get 401", pool, strings.Join(quoted, ", "))
}

// keyedVerdictMessage composes the BackendAPIKeyUnsupported message for both
// keying paths: backends whose own InferenceService requires a key, and
// backends keyed only through a pool sibling. The reference-only case reads
// exactly like keyedBackendsMessage.
func keyedVerdictMessage(names, poolNames []string) string {
	quoted := func(list []string) string {
		out := make([]string, len(list))
		for i, n := range list {
			out[i] = fmt.Sprintf("%q", n)
		}
		return strings.Join(out, ", ")
	}
	var cause string
	switch {
	case len(names) > 0 && len(poolNames) > 0:
		cause = fmt.Sprintf("backend(s) %s point at an InferenceService that requires an API key "+
			"(spec.endpoint.apiKeySecretRef, --api-key, or VLLM_API_KEY in spec.env (vLLM)), and backend(s) %s front a pool containing one",
			quoted(names), quoted(poolNames))
	case len(poolNames) > 0:
		cause = fmt.Sprintf("backend(s) %s front a ModelPool containing a member whose InferenceService "+
			"requires an API key (spec.endpoint.apiKeySecretRef, --api-key, or VLLM_API_KEY in spec.env (vLLM))", quoted(poolNames))
	default:
		cause = fmt.Sprintf("backend(s) %s point at an InferenceService that requires an API key "+
			"(spec.endpoint.apiKeySecretRef, --api-key, or VLLM_API_KEY in spec.env (vLLM))", quoted(names))
	}
	return cause + "; ModelRouter does not forward that key yet, so requests would get 401. " +
		"Remove the key from the InferenceService or route to it directly"
}

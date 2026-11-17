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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// deploymentFor renders the Deployment a test asserts on. Production callers
// pass the plan the reconcile warned about; a test that renders a service
// directly derives it here with runtimeCachePlan, which reads no claim, so a
// case whose claim changes the plan passes it explicitly.
//
//nolint:unparam // draftHFEndpoint is parameterized to mirror the builder's signature even though no test passes one
func (r *InferenceServiceReconciler) deploymentFor(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	draftModel *inferencev1alpha1.Model,
	replicas int32,
	hfEndpoint string,
	draftHFEndpoint string,
) *appsv1.Deployment {
	return r.constructDeploymentForCache(isvc, model, cache, draftModel, replicas, hfEndpoint, draftHFEndpoint,
		r.runtimeCachePlan(isvc, model, cache, resolveBackend(isvc)))
}

// memberPodsFor is deploymentFor for the multiNode path.
//
//nolint:unparam // the draft and endpoint parameters mirror the builder's signature even though no test passes one
func (r *InferenceServiceReconciler) memberPodsFor(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	cache effectiveModelCache,
	draftModel *inferencev1alpha1.Model,
	hfEndpoint string,
	draftHFEndpoint string,
) ([]*corev1.Pod, string, error) {
	return r.constructMemberPods(isvc, model, cache, draftModel, hfEndpoint, draftHFEndpoint,
		r.runtimeCachePlan(isvc, model, cache, resolveBackend(isvc)))
}

// warnRuntimeCacheFor emits the runtime-cache warning a reconcile of isvc would
// emit for model.
func warnRuntimeCacheFor(r *InferenceServiceReconciler, isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) {
	r.warnRuntimeCacheNotApplied(isvc,
		r.runtimeCachePlanFor(context.Background(), isvc, model, resolveEffectiveModelCache(isvc, model), resolveBackend(isvc)))
}

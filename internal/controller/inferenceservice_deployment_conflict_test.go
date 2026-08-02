/*
Copyright 2025.

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
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestReconcileDeploymentRetriesOnConflict verifies the InferenceService
// reconciler absorbs an optimistic-lock conflict on the Deployment update
// rather than surfacing it as an error. A ModelPool drives a swap by patching a
// member's spec.replicas, which advances the member Deployment's
// resourceVersion while the isvc reconciler is also updating that Deployment;
// without a retry the reconciler loses the race and logs a spurious "Failed to
// update Deployment" on every swap.
func TestReconcileDeploymentRetriesOnConflict(t *testing.T) {
	const modelName = "dep-conflict-model"
	const serviceName = "dep-conflict-svc"
	const ns = testBuilderNs

	scheme := builderTestScheme(t)

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: ns},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       "https://huggingface.co/test/model.gguf",
			Format:       "gguf",
			Quantization: "Q4_K_M",
			Hardware:     &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
			Resources:    &inferencev1alpha1.ResourceRequirements{CPU: "1", Memory: "1Gi"},
		},
	}
	replicas := int32(1)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: modelName,
			Replicas: &replicas,
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	}
	// Existing Deployment already carries the current operator selector so the
	// reconcile takes the in-place update path (not the selector-migration
	// recreate path).
	sel := map[string]string{
		"app":                           serviceName,
		"inference.llmkube.dev/service": serviceName,
	}
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "server",
					Image: "ghcr.io/ggml-org/llama.cpp:server",
				}}},
			},
		},
	}

	var conflicts atomic.Int32
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(model, isvc, existing).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ictx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				// Inject exactly one conflict, on the first Deployment update.
				if _, ok := obj.(*appsv1.Deployment); ok && conflicts.Add(1) == 1 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "apps", Resource: "deployments"},
						obj.GetName(),
						fmt.Errorf("the object has been modified; please apply your changes to the latest version"),
					)
				}
				return cl.Update(ictx, obj, opts...)
			},
		}).
		Build()

	r := &InferenceServiceReconciler{
		Client:             c,
		Scheme:             scheme,
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
	}

	if _, _, _, _, err := r.reconcileDeployment(context.Background(), isvc, model, 1, true, false); err != nil {
		t.Fatalf("reconcileDeployment surfaced a conflict instead of retrying: %v", err)
	}

	if got := conflicts.Load(); got < 2 {
		t.Errorf("Deployment update attempts = %d, want >= 2 (initial conflict + retry)", got)
	}

	updated := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: serviceName, Namespace: ns}, updated); err != nil {
		t.Fatalf("get updated Deployment: %v", err)
	}
	if _, ok := updated.Annotations[AnnotationDesiredTemplateHash]; !ok {
		t.Error("desired-template hash not stamped; the retried update did not land")
	}
}

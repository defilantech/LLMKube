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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestMarkPlanned_WebhookRejectionIsTerminal proves Fix 2 (#520 webhook
// requeue regression): once the validating webhook is live, a Workload
// whose step agentRef names an Agent that never exists has its child
// AgenticTask CREATE rejected by the webhook with an IsInvalid admission
// error on every reconcile. The reconciler must treat that as TERMINAL
// (Phase=Failed, no requeue error) rather than looping forever, while a
// transient NotFound create error must STILL requeue.
//
// This is a focused controller-level test using a fake client with a
// Create interceptor, not envtest: the foreman controller envtest suite
// does not install the admission webhook, so the IsInvalid rejection
// cannot be produced through the real API server here. The interceptor
// reproduces the exact error shape (apierrors.NewInvalid, optionally
// wrapped the way renderAndCreate wraps it) that the live webhook returns.
func TestMarkPlanned_WebhookRejectionIsTerminal(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add foreman scheme: %v", err)
	}

	agentGK := schema.GroupKind{Group: foremanv1alpha1.GroupVersion.Group, Kind: "AgenticTask"}

	tests := []struct {
		name         string
		createErr    error
		wantTerminal bool // expect Phase=Failed and no requeue error
	}{
		{
			name: "IsInvalid admission rejection is terminal",
			// Shaped like the webhook's rejection, then wrapped the way
			// renderAndCreate wraps create errors, to prove IsInvalid
			// survives the %w wrapping.
			createErr: fmt.Errorf("create AgenticTask %q: %w", "wl-step-a",
				apierrors.NewInvalid(agentGK, "wl-step-a", field.ErrorList{
					field.Invalid(field.NewPath("spec", "agentRef"), "ghost",
						"agentRef names an Agent that does not exist"),
				})),
			wantTerminal: true,
		},
		{
			name: "NotFound create error is transient and requeues",
			createErr: fmt.Errorf("create AgenticTask %q: %w", "wl-step-a",
				apierrors.NewNotFound(schema.GroupResource{Group: foremanv1alpha1.GroupVersion.Group, Resource: "agentictasks"}, "wl-step-a")),
			wantTerminal: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wl := newWorkload("wl", foremanv1alpha1.WorkloadSpec{
				Intent: "explicit",
				Repo:   "defilantech/LLMKube",
				Pipeline: []foremanv1alpha1.PipelineStep{
					{
						Name:     "step-a",
						Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
						AgentRef: corev1.LocalObjectReference{Name: "ghost"},
						Payload:  foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 1},
					},
				},
			})

			createErr := tt.createErr
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(wl).
				WithStatusSubresource(&foremanv1alpha1.Workload{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
						if _, ok := obj.(*foremanv1alpha1.AgenticTask); ok {
							return createErr
						}
						return nil
					},
				}).
				Build()

			r := &WorkloadReconciler{
				Client:              fakeClient,
				Scheme:              scheme,
				AllowCloudProviders: true,
			}

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(wl)})

			var fresh foremanv1alpha1.Workload
			if getErr := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(wl), &fresh); getErr != nil {
				t.Fatalf("get workload: %v", getErr)
			}

			if tt.wantTerminal {
				if err != nil {
					t.Fatalf("terminal rejection must not return a requeue error, got %v", err)
				}
				if res.Requeue || res.RequeueAfter != 0 {
					t.Fatalf("terminal rejection must not request a requeue, got %+v", res)
				}
				if fresh.Status.Phase != foremanv1alpha1.WorkloadPhaseFailed {
					t.Fatalf("expected Phase=%s, got %s", foremanv1alpha1.WorkloadPhaseFailed, fresh.Status.Phase)
				}
			} else {
				// Transient: the reconcile returns the error so the
				// manager requeues, and the Workload is NOT terminal.
				if err == nil {
					t.Fatalf("transient create failure must return an error to requeue")
				}
				if fresh.Status.Phase == foremanv1alpha1.WorkloadPhaseFailed {
					t.Fatalf("transient create failure must NOT drive the Workload to a terminal Failed phase")
				}
			}
		})
	}
}

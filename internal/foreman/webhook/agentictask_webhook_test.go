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

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func taskScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add foreman to scheme: %v", err)
	}
	return s
}

func newAgent(name, ns string) *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: foremanv1alpha1.AgentSpec{
			Role:                foremanv1alpha1.AgentRoleCoder,
			InferenceServiceRef: corev1.LocalObjectReference{Name: "qwen"},
			SystemPrompt:        "You are a coder.",
			Tools:               []string{"read_file", "submit_result"},
		},
	}
}

func TestAgenticTaskValidator_Create(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name      string
		objs      []client.Object
		task      *foremanv1alpha1.AgenticTask
		wantError bool
	}{
		{
			name: "task with no agentRef accepted (capability-only)",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "stub", Namespace: "default"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind: foremanv1alpha1.AgenticTaskKindIssueFix,
					RequiredCapability: foremanv1alpha1.RequiredCapability{
						Roles: []string{"worker"},
					},
				},
			},
			wantError: false,
		},
		{
			name: "task with empty agentRef name accepted (capability-only)",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "stub2", Namespace: "default"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: &corev1.LocalObjectReference{Name: ""},
				},
			},
			wantError: false,
		},
		{
			name: "task with resolvable agentRef accepted",
			objs: []client.Object{newAgent("coder", "default")},
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "default"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: &corev1.LocalObjectReference{Name: "coder"},
				},
			},
			wantError: false,
		},
		{
			name: "task with unresolvable agentRef rejected",
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "default"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: &corev1.LocalObjectReference{Name: "missing"},
				},
			},
			wantError: true,
		},
		{
			name: "agentRef resolves only in the same namespace (cross-ns rejected)",
			objs: []client.Object{newAgent("coder", "other")},
			task: &foremanv1alpha1.AgenticTask{
				ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "default"},
				Spec: foremanv1alpha1.AgenticTaskSpec{
					Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
					AgentRef: &corev1.LocalObjectReference{Name: "coder"},
				},
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().
				WithScheme(taskScheme(t)).
				WithObjects(tt.objs...).
				Build()
			v := &AgenticTaskValidator{Client: c}
			_, err := v.ValidateCreate(ctx, tt.task)
			if tt.wantError && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tt.wantError && err != nil && !apierrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got %T: %v", err, err)
			}
		})
	}
}

func TestAgenticTaskValidator_NoopUpdateDelete(t *testing.T) {
	v := &AgenticTaskValidator{Client: fake.NewClientBuilder().WithScheme(taskScheme(t)).Build()}
	ctx := context.Background()
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "fix", Namespace: "default"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			AgentRef: &corev1.LocalObjectReference{Name: "missing"},
		},
	}
	// Even an unresolvable agentRef passes update/delete (no-ops).
	if _, err := v.ValidateUpdate(ctx, task, task); err != nil {
		t.Fatalf("ValidateUpdate should be a no-op, got %v", err)
	}
	if _, err := v.ValidateDelete(ctx, task); err != nil {
		t.Fatalf("ValidateDelete should be a no-op, got %v", err)
	}
}

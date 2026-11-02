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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// TestExternalReplicasManager covers the managedFields shapes the envtest
// specs do not reach: managers from builds before #2302, co-ownership by an
// Apply, and a scale subresource entry with no time. The FieldsV1 bodies are
// the API server's encoding as envtest returned it.
func TestExternalReplicasManager(t *testing.T) {
	replicas := `{"f:spec":{"f:replicas":{}}}`
	other := `{"f:spec":{"f:image":{},"f:modelRef":{}}}`
	at := func(sec int) *metav1.Time {
		tm := metav1.NewTime(time.Unix(1_800_000_000+int64(sec), 0))
		return &tm
	}
	entry := func(manager, fields string, tm *metav1.Time) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Time: tm, FieldsV1: metav1.NewFieldsV1(fields)}
	}

	tests := []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    string
	}{
		{"no owner, a CRD default", []metav1.ManagedFieldsEntry{entry("kustomize-controller", other, at(0))}, ""},
		{"the pool", []metav1.ManagedFieldsEntry{entry(inferencev1alpha1.ModelPoolFieldManager, replicas, at(0))}, ""},
		{"the router", []metav1.ManagedFieldsEntry{entry(inferencev1alpha1.RouterProxyFieldManager, replicas, at(0))}, ""},
		{"an operator from before #2302", []metav1.ManagedFieldsEntry{entry("manager", replicas, at(0))}, ""},
		{"a router-proxy from before #2302", []metav1.ManagedFieldsEntry{entry("router-proxy", replicas, at(0))}, ""},
		{"an outside owner of other fields only", []metav1.ManagedFieldsEntry{
			entry("kustomize-controller", other, at(5)),
			entry(inferencev1alpha1.RouterProxyFieldManager, replicas, at(1)),
		}, ""},
		{"an Apply co-owning the router's value", []metav1.ManagedFieldsEntry{
			entry(inferencev1alpha1.RouterProxyFieldManager, replicas, at(5)),
			entry("kustomize-controller", replicas, at(1)),
		}, "kustomize-controller"},
		{"a scale subresource entry carries no time", []metav1.ManagedFieldsEntry{
			entry("kubectl", replicas, nil),
		}, "kubectl"},
		{"a timed outside owner beats an untimed one", []metav1.ManagedFieldsEntry{
			entry("kubectl", replicas, nil),
			entry("argocd-controller", replicas, at(1)),
		}, "argocd-controller"},
		{"the latest outside owner wins", []metav1.ManagedFieldsEntry{
			entry("argocd-controller", replicas, at(5)),
			entry("kustomize-controller", replicas, at(1)),
		}, "argocd-controller"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{ManagedFields: tc.entries}}
			if got := externalReplicasManager(isvc); got != tc.want {
				t.Errorf("externalReplicasManager = %q, want %q", got, tc.want)
			}
		})
	}
}

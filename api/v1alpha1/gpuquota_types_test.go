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

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fullyPopulatedGPUQuota returns a GPUQuota that exercises every non-trivial
// spec and status path. Used as the canonical test fixture so round-trip and
// deep-copy coverage stays in sync as the type evolves.
func fullyPopulatedGPUQuota() *GPUQuota {
	return &GPUQuota{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inference.llmkube.dev/v1alpha1",
			Kind:       "GPUQuota",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "platform-gpu-quota",
			Namespace: "platform",
		},
		Spec: GPUQuotaSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"team": "ml"},
			},
			GPUCount:      8,
			VRAMBytes:     512 * 1024 * 1024 * 1024,
			MinPriority:   "high",
			CostBudgetRef: "platform-budget",
		},
		Status: GPUQuotaStatus{
			UsedGPUCount:     3,
			UsedVRAMBytes:    192 * 1024 * 1024 * 1024,
			AdmissionDenials: 7,
			LastDenial:       &metav1.Time{Time: time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)},
		},
	}
}

// TestGPUQuotaDeepCopyIndependence verifies the generated DeepCopy methods
// produce a fully independent clone: mutating slices, maps, and pointer
// fields on the copy must not be visible on the original.
func TestGPUQuotaDeepCopyIndependence(t *testing.T) {
	orig := fullyPopulatedGPUQuota()
	clone := orig.DeepCopy()

	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("clone differs from original immediately after DeepCopy")
	}

	// Mutate pointer fields on the clone.
	clone.Spec.Selector.MatchLabels["team"] = "MUTATED"
	clone.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"team": "other"},
	}

	if got := orig.Spec.Selector.MatchLabels["team"]; got != "ml" {
		t.Errorf("original Spec.Selector.MatchLabels[\"team\"] = %q; want %q", got, "ml")
	}

	clone.Status.LastDenial = &metav1.Time{Time: time.Time{}}
	if orig.Status.LastDenial == nil {
		t.Error("original Status.LastDenial pointer was overwritten by clone mutation")
	}

	clone.Status.LastDenial = nil
	if orig.Status.LastDenial == nil {
		t.Error("original Status.LastDenial is nil after clone set to nil")
	}
}

// TestGPUQuotaJSONRoundTrip confirms every field marshals and unmarshals
// through JSON without loss. This catches missing or incorrect json tags.
func TestGPUQuotaJSONRoundTrip(t *testing.T) {
	orig := fullyPopulatedGPUQuota()

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got GPUQuota
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Use a custom comparison because metav1.Time marshals to RFC3339
	// without a zone suffix, so the decoded Time lands in local zone.
	// Compare every field except LastDenial.Time directly; for that one
	// compare via Equal().
	if !reflect.DeepEqual(orig.TypeMeta, got.TypeMeta) {
		t.Fatalf("TypeMeta mismatch")
	}
	if orig.Name != got.Name || orig.Namespace != got.Namespace {
		t.Fatalf("ObjectMeta mismatch: %#v vs %#v", orig.ObjectMeta, got.ObjectMeta)
	}
	if !reflect.DeepEqual(orig.Spec, got.Spec) {
		t.Fatalf("Spec mismatch: %#v vs %#v", orig.Spec, got.Spec)
	}
	if orig.Status.UsedGPUCount != got.Status.UsedGPUCount ||
		orig.Status.UsedVRAMBytes != got.Status.UsedVRAMBytes ||
		orig.Status.AdmissionDenials != got.Status.AdmissionDenials {
		t.Fatalf("Status numeric fields mismatch: %#v vs %#v", orig.Status, got.Status)
	}
	if (orig.Status.LastDenial == nil) != (got.Status.LastDenial == nil) {
		t.Fatalf("LastDenial nil mismatch: orig=%v got=%v", orig.Status.LastDenial, got.Status.LastDenial)
	}
	if orig.Status.LastDenial != nil && !orig.Status.LastDenial.Time.Equal(got.Status.LastDenial.Time) {
		t.Fatalf("LastDenial.Time mismatch: %v vs %v", orig.Status.LastDenial.Time, got.Status.LastDenial.Time)
	}
}

// TestGPUQuotaMinimalSpec verifies a minimal GPUQuota (only required fields)
// marshals and unmarshals correctly.
func TestGPUQuotaMinimalSpec(t *testing.T) {
	q := &GPUQuota{
		Spec: GPUQuotaSpec{
			GPUCount: 4,
		},
	}
	data, err := json.Marshal(q.Spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GPUQuotaSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.GPUCount != 4 {
		t.Errorf("GPUCount = %d; want 4", got.GPUCount)
	}
	if got.Selector != nil {
		t.Errorf("Selector = %v; want nil", got.Selector)
	}
	if got.NamespaceRef != "" {
		t.Errorf("NamespaceRef = %q; want empty", got.NamespaceRef)
	}
}

// TestGPUQuotaNamespaceRefOnly verifies NamespaceRef-only mode round-trips.
func TestGPUQuotaNamespaceRefOnly(t *testing.T) {
	q := &GPUQuota{
		Spec: GPUQuotaSpec{
			NamespaceRef: "ml-team",
			GPUCount:     2,
		},
	}
	data, err := json.Marshal(q.Spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GPUQuotaSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.NamespaceRef != "ml-team" {
		t.Errorf("NamespaceRef = %q; want %q", got.NamespaceRef, "ml-team")
	}
	if got.Selector != nil {
		t.Errorf("Selector = %v; want nil", got.Selector)
	}
}

// TestGPUQuotaListDeepCopy verifies the list type's DeepCopy is independent.
func TestGPUQuotaListDeepCopy(t *testing.T) {
	list := &GPUQuotaList{
		Items: []GPUQuota{
			*fullyPopulatedGPUQuota(),
		},
	}
	clone := list.DeepCopy()

	if !reflect.DeepEqual(list, clone) {
		t.Fatal("GPUQuotaList clone differs from original")
	}

	clone.Items[0].Spec.GPUCount = 0
	if got := list.Items[0].Spec.GPUCount; got != 8 {
		t.Errorf("original Items[0].Spec.GPUCount = %d; want 8", got)
	}
}

// TestGPUQuotaSchemeRegistration confirms the GPUQuota and GPUQuotaList types
// are registered with the package's SchemeBuilder (i.e. init() ran).
func TestGPUQuotaSchemeRegistration(t *testing.T) {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("SchemeBuilder.Build: %v", err)
	}
	gvks, _, err := scheme.ObjectKinds(&GPUQuota{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(GPUQuota): %v", err)
	}
	if len(gvks) == 0 {
		t.Fatal("GPUQuota not registered in scheme")
	}
	if gvks[0].Group != "inference.llmkube.dev" || gvks[0].Version != "v1alpha1" || gvks[0].Kind != "GPUQuota" {
		t.Errorf("unexpected GVK %s", gvks[0])
	}
}

// TestGPUQuotaPriorityEnum verifies the documented priority values are
// accepted by the type.
func TestGPUQuotaPriorityEnum(t *testing.T) {
	priorities := []string{"critical", "high", "normal", "low", "batch"}
	for _, p := range priorities {
		t.Run(p, func(t *testing.T) {
			q := &GPUQuota{
				Spec: GPUQuotaSpec{
					GPUCount:    1,
					MinPriority: p,
				},
			}
			data, err := json.Marshal(q.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got GPUQuotaSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.MinPriority != p {
				t.Errorf("MinPriority = %q; want %q", got.MinPriority, p)
			}
		})
	}
}

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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestDetectUnbindableVolume(t *testing.T) {
	// From a pod blocked by defilantech/LLMKube#1509, with the node name replaced
	// by a placeholder: the microk8s hostpath provisioner's helper could not
	// schedule onto a GPU-tainted node, so it never stamped the PV, and the PV
	// was published with a node affinity matching no node.
	const realMessage = `running PreBind plugin "VolumeBinding": binding volumes: ` +
		`pv "pvc-7d8e47dd-1b2c-4a3e-9f10-2c3d4e5f6a7b" node affinity doesn't match node "spark-2": no matching NodeSelectorTerms`

	t.Run("detects the PreBind volume affinity failure", func(t *testing.T) {
		pv, node, ok := detectUnbindableVolume(realMessage)
		if !ok {
			t.Fatalf("detectUnbindableVolume() ok = false, want true")
		}
		if pv != "pvc-7d8e47dd-1b2c-4a3e-9f10-2c3d4e5f6a7b" {
			t.Fatalf("detectUnbindableVolume() pv = %q, want the pvc- name", pv)
		}
		if node != "spark-2" {
			t.Fatalf("detectUnbindableVolume() node = %q, want %q", node, "spark-2")
		}
	})

	t.Run("ignores an ordinary insufficient-resource message", func(t *testing.T) {
		if _, _, ok := detectUnbindableVolume("0/4 nodes are available: 1 Insufficient nvidia.com/gpu."); ok {
			t.Fatalf("detectUnbindableVolume() ok = true, want false for a GPU shortage")
		}
	})

	t.Run("ignores an ordinary taint message", func(t *testing.T) {
		msg := "0/4 nodes are available: 2 node(s) had untolerated taint(s)."
		if _, _, ok := detectUnbindableVolume(msg); ok {
			t.Fatalf("detectUnbindableVolume() ok = true, want false for a taint mismatch")
		}
	})

	t.Run("does not fire on a volume message that is not the affinity failure", func(t *testing.T) {
		msg := `running PreBind plugin "VolumeBinding": binding volumes: timed out waiting for the condition`
		if _, _, ok := detectUnbindableVolume(msg); ok {
			t.Fatalf("detectUnbindableVolume() ok = true, want false for an unrelated VolumeBinding error")
		}
	})

	t.Run("tolerates a missing node name", func(t *testing.T) {
		msg := `binding volumes: pv "pvc-abc" node affinity doesn't match node: no matching NodeSelectorTerms`
		pv, node, ok := detectUnbindableVolume(msg)
		if !ok {
			t.Fatalf("detectUnbindableVolume() ok = false, want true")
		}
		if pv != "pvc-abc" {
			t.Fatalf("detectUnbindableVolume() pv = %q, want %q", pv, "pvc-abc")
		}
		if node != "" {
			t.Fatalf("detectUnbindableVolume() node = %q, want empty", node)
		}
	})
}

func TestUnbindableVolumeMessage(t *testing.T) {
	msg := unbindableVolumeMessage("pvc-123", "spark-2", "spark-cache")

	// The whole point of the change is that the operator says something the
	// scheduler's own text does not: that this is a provisioning failure, and
	// which claim it belongs to. Assert on that, not on exact prose.
	for _, want := range []string{"pvc-123", "spark-2", "spark-cache"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("unbindableVolumeMessage() = %q, want it to mention %q", msg, want)
		}
	}
	if !strings.Contains(strings.ToLower(msg), "provision") {
		t.Fatalf("unbindableVolumeMessage() = %q, want it to name provisioning as the cause", msg)
	}
}

// The classifier is only useful if it survives the path a real reconcile takes:
// a Pending pod with PodScheduled=False must come back as UnbindableModelCache
// rather than the scheduler's raw text.
func TestGetPodSchedulingInfoReportsUnbindableModelCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("inferencev1alpha1.AddToScheme: %v", err)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "spark-svc", Namespace: "default"},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spark-svc-abc",
			Namespace: "default",
			Labels: map[string]string{
				"app":                           "spark-svc",
				"inference.llmkube.dev/service": "spark-svc",
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "model-cache",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "spark-svc-model-cache",
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: "SchedulerError",
				Message: `running PreBind plugin "VolumeBinding": binding volumes: ` +
					`pv "pvc-7d8e47dd" node affinity doesn't match node "spark-2": no matching NodeSelectorTerms`,
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(isvc, pod, unstampedClaim(), unstampedPV()).Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

	info, err := r.getPodSchedulingInfo(context.Background(), isvc)
	if err != nil {
		t.Fatalf("getPodSchedulingInfo() error = %v", err)
	}
	if info == nil {
		t.Fatalf("getPodSchedulingInfo() = nil, want scheduling info")
	}
	if info.Status != "UnbindableModelCache" {
		t.Fatalf("Status = %q, want %q", info.Status, "UnbindableModelCache")
	}
	if !strings.Contains(info.Message, "spark-svc-model-cache") {
		t.Fatalf("Message = %q, want it to name the claim", info.Message)
	}
	// The raw scheduler wording is what misled us for two days; it must not be
	// what the user is left holding.
	if strings.Contains(info.Message, "no matching NodeSelectorTerms") {
		t.Fatalf("Message = %q, should not pass through the raw scheduler text", info.Message)
	}
}

// Regression for the gap that shipped in #1511: getPodSchedulingInfo classified
// the failure correctly, but determinePhase forwarded scheduling info only for
// InsufficientGPU, so the diagnosis was computed and then dropped. Asserting at
// the determinePhase level is what catches that; the earlier test could not.
func TestDeterminePhaseSurfacesUnbindableModelCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("appsv1.AddToScheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("inferencev1alpha1.AddToScheme: %v", err)
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "spark-svc", Namespace: "default"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "spark-svc-abc",
			Namespace: "default",
			Labels: map[string]string{
				"app":                           "spark-svc",
				"inference.llmkube.dev/service": "spark-svc",
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "model-cache",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "spark-svc-model-cache",
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionFalse,
				Reason: "SchedulerError",
				Message: `running PreBind plugin "VolumeBinding": binding volumes: ` +
					`pv "pvc-7d8e47dd" node affinity doesn't match node "spark-2": no matching NodeSelectorTerms`,
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(isvc, pod, unstampedClaim(), unstampedPV()).Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

	_, info := r.determinePhase(context.Background(), isvc, 0, 1, false, &appsv1.Deployment{}, nil)
	if info == nil {
		t.Fatalf("determinePhase() scheduling info = nil; the diagnosis was computed and dropped")
	}
	if info.Status != "UnbindableModelCache" {
		t.Fatalf("Status = %q, want %q", info.Status, "UnbindableModelCache")
	}
	if !strings.Contains(info.Message, "spark-svc-model-cache") {
		t.Fatalf("Message = %q, want it to name the claim", info.Message)
	}
}

// unstampedClaim/unstampedPV mirror the objects observed on the cluster while
// reproducing #1509: the claim is bound to a volume the provisioner created but
// never stamped, so its hostname term holds a single empty string.
func unstampedClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "spark-svc-model-cache", Namespace: "default"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-7d8e47dd"},
	}
}

func unstampedPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-7d8e47dd"},
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "kubernetes.io/hostname",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{""},
						}},
					}},
				},
			},
		},
	}
}

func TestUnsatisfiableNodeAffinity(t *testing.T) {
	t.Run("empty hostname value is unsatisfiable", func(t *testing.T) {
		if !unsatisfiableNodeAffinity(unstampedPV()) {
			t.Fatalf("unsatisfiableNodeAffinity() = false, want true for values [\"\"]")
		}
	})

	t.Run("a real hostname is satisfiable", func(t *testing.T) {
		pv := unstampedPV()
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"spark-1"}
		if unsatisfiableNodeAffinity(pv) {
			t.Fatalf("unsatisfiableNodeAffinity() = true, want false for a real hostname")
		}
	})

	t.Run("no affinity at all is satisfiable", func(t *testing.T) {
		if unsatisfiableNodeAffinity(&corev1.PersistentVolume{}) {
			t.Fatalf("unsatisfiableNodeAffinity() = true, want false when there is no affinity")
		}
	})
}

// The wording a freshly created InferenceService actually produces, captured
// verbatim from the cluster while reproducing #1509. The first cut of this code
// only knew the PreBind form and stayed silent on this one.
func TestFilterFormIsClassified(t *testing.T) {
	const filterMessage = "0/4 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity, " +
		"3 node(s) didn't match Pod's node affinity/selector. no new claims to deallocate, " +
		"preemption: 0/4 nodes are available: 4 Preemption is not helpful for scheduling."

	if _, _, ok := detectUnbindableVolume(filterMessage); !ok {
		t.Fatalf("detectUnbindableVolume() ok = false, want true for the Filter wording")
	}
}

// A PV pinned to a real node produces the same scheduler wording but is a
// different problem; asserting a provisioning failure there would mislead.
func TestGenuinelyPinnedVolumeIsNotReclassified(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, inferencev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("AddToScheme: %v", err)
		}
	}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "spark-svc", Namespace: "default"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "spark-svc-abc", Namespace: "default",
			Labels: map[string]string{"app": "spark-svc", "inference.llmkube.dev/service": "spark-svc"},
		},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "model-cache",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "spark-svc-model-cache",
			}},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
				Message: "0/4 nodes are available: 1 node(s) didn't match PersistentVolume's node affinity.",
			}},
		},
	}
	pinned := unstampedPV()
	pinned.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values = []string{"spark-2"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isvc, pod, unstampedClaim(), pinned).Build()
	r := &InferenceServiceReconciler{Client: c, Scheme: scheme}

	info, err := r.getPodSchedulingInfo(context.Background(), isvc)
	if err != nil {
		t.Fatalf("getPodSchedulingInfo() error = %v", err)
	}
	if info != nil && info.Status == "UnbindableModelCache" {
		t.Fatalf("a PV pinned to a real node was reported as a provisioning failure: %q", info.Message)
	}
}

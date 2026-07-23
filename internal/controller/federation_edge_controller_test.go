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
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func gpuNode(name string, gpus int64) corev1.Node {
	qty := *resource.NewQuantity(gpus, resource.DecimalSI)
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				nvidiaGPUResourceName: qty,
			},
			Allocatable: corev1.ResourceList{
				nvidiaGPUResourceName: qty,
			},
		},
	}
}

func TestBuildStatusSummary(t *testing.T) {
	models := []inferencev1alpha1.Model{
		{ObjectMeta: metav1.ObjectMeta{Name: "model-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "model-b"}},
	}
	isvcs := []inferencev1alpha1.InferenceService{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "isvc-a"},
			Status:     inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "isvc-b"},
			Status:     inferencev1alpha1.InferenceServiceStatus{Phase: PhaseReady},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "isvc-c"},
			Status:     inferencev1alpha1.InferenceServiceStatus{Phase: PhaseFailed},
		},
	}
	nodes := []corev1.Node{
		gpuNode("node-a", 2),
		gpuNode("node-b", 4),
	}

	got := buildStatusSummary(models, isvcs, nodes, "0.9.10")

	if got.Phase != "" {
		t.Errorf("Phase = %q, want empty (datacenter-owned)", got.Phase)
	}
	if got.LastHeartbeatTime != nil {
		t.Errorf("LastHeartbeatTime = %v, want nil (stamped by the caller at push time)", got.LastHeartbeatTime)
	}
	if got.ObservedVersion != "0.9.10" {
		t.Errorf("ObservedVersion = %q, want %q", got.ObservedVersion, "0.9.10")
	}
	if got.Inference == nil {
		t.Fatal("Inference summary is nil")
	}
	if got.Inference.Models != 2 {
		t.Errorf("Models = %d, want 2", got.Inference.Models)
	}
	if got.Inference.ServicesReady != 2 {
		t.Errorf("ServicesReady = %d, want 2", got.Inference.ServicesReady)
	}
	if got.Inference.ServicesFailed != 1 {
		t.Errorf("ServicesFailed = %d, want 1", got.Inference.ServicesFailed)
	}
	if got.Inference.ServicesTotal != 3 {
		t.Errorf("ServicesTotal = %d, want 3", got.Inference.ServicesTotal)
	}
	if got.Capacity == nil {
		t.Fatal("Capacity is nil")
	}
	if got.Capacity.Nodes != 2 {
		t.Errorf("Nodes = %d, want 2", got.Capacity.Nodes)
	}
	if got.Capacity.GPUsTotal != 6 {
		t.Errorf("GPUsTotal = %d, want 6", got.Capacity.GPUsTotal)
	}
	if got.Capacity.GPUsAllocatable != 6 {
		t.Errorf("GPUsAllocatable = %d, want 6", got.Capacity.GPUsAllocatable)
	}
}

func TestBuildStatusSummaryEmpty(t *testing.T) {
	got := buildStatusSummary(nil, nil, nil, "")
	if got.Phase != "" {
		t.Errorf("Phase = %q, want empty", got.Phase)
	}
	if got.Inference.ServicesTotal != 0 || got.Inference.Models != 0 {
		t.Errorf("expected zeroed inference summary, got %+v", got.Inference)
	}
	if got.Capacity.Nodes != 0 || got.Capacity.GPUsTotal != 0 {
		t.Errorf("expected zeroed capacity, got %+v", got.Capacity)
	}
}

// --- envtest push spec: a single tick against an envtest apiserver acting
// as both the local cluster (reads) and the datacenter (status write). ---

var _ = Describe("FederationEdgeReconciler", func() {
	const clusterName = "edge-fed-test"

	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()

		fc := &federationv1alpha1.FederatedCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName},
			Spec:       federationv1alpha1.FederatedClusterSpec{HeartbeatIntervalSeconds: 30},
		}
		Expect(k8sClient.Create(ctx, fc)).To(Succeed())

		model := &inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-model", Namespace: "default"},
			Spec:       inferencev1alpha1.ModelSpec{Source: "/models/edge.gguf"},
		}
		Expect(k8sClient.Create(ctx, model)).To(Succeed())

		replicas := int32(1)
		isvcReady := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-isvc-ready", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, Replicas: &replicas},
		}
		Expect(k8sClient.Create(ctx, isvcReady)).To(Succeed())
		isvcReady.Status.Phase = PhaseReady
		Expect(k8sClient.Status().Update(ctx, isvcReady)).To(Succeed())

		isvcFailed := &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-isvc-failed", Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, Replicas: &replicas},
		}
		Expect(k8sClient.Create(ctx, isvcFailed)).To(Succeed())
		isvcFailed.Status.Phase = PhaseFailed
		Expect(k8sClient.Status().Update(ctx, isvcFailed)).To(Succeed())
	})

	AfterEach(func() {
		fc := &federationv1alpha1.FederatedCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName}, fc); err == nil {
			Expect(k8sClient.Delete(ctx, fc)).To(Succeed())
		}
		for _, name := range []string{"edge-isvc-ready", "edge-isvc-failed"} {
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, isvc); err == nil {
				Expect(k8sClient.Delete(ctx, isvc)).To(Succeed())
			}
		}
		model := &inferencev1alpha1.Model{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "edge-model", Namespace: "default"}, model); err == nil {
			Expect(k8sClient.Delete(ctx, model)).To(Succeed())
		}
	})

	It("pushes the observed summary to the datacenter FederatedCluster status without touching Phase", func() {
		// Seed a datacenter-owned Phase before the push, to prove the edge
		// patch leaves it alone.
		fc := &federationv1alpha1.FederatedCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName}, fc)).To(Succeed())
		fc.Status.Phase = federationv1alpha1.FederatedClusterConnected
		Expect(k8sClient.Status().Update(ctx, fc)).To(Succeed())

		reconciler := &FederationEdgeReconciler{
			LocalClient:      k8sClient,
			DatacenterClient: k8sClient,
			ClusterName:      clusterName,
			Version:          "0.9.10-test",
			Interval:         time.Hour,
		}
		reconciler.tick(ctx, logr.Discard())

		got := &federationv1alpha1.FederatedCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName}, got)).To(Succeed())

		Expect(got.Status.Phase).To(Equal(federationv1alpha1.FederatedClusterConnected), "edge push must never clobber the datacenter-owned Phase")
		Expect(got.Status.LastHeartbeatTime).NotTo(BeNil())
		Expect(got.Status.ObservedVersion).To(Equal("0.9.10-test"))
		Expect(got.Status.Inference).NotTo(BeNil())
		Expect(got.Status.Inference.ServicesReady).To(Equal(int32(1)))
		Expect(got.Status.Inference.ServicesFailed).To(Equal(int32(1)))
		Expect(got.Status.Inference.ServicesTotal).To(Equal(int32(2)))
		Expect(got.Status.Inference.Models).To(Equal(int32(1)))
		Expect(got.Status.Capacity).NotTo(BeNil())
	})
})

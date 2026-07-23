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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	federationv1alpha1 "github.com/defilantech/llmkube/api/federation/v1alpha1"
)

func TestPhaseForHeartbeat(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	iv := int32(30)
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"fresh", 10 * time.Second, "Connected"},
		{"just under 3x", 89 * time.Second, "Connected"},
		{"at stale edge", 4 * time.Minute, "Stale"},
		{"just under 10x", 299 * time.Second, "Stale"},
		{"unreachable", 6 * time.Minute, "Unreachable"},
	}
	for _, c := range cases {
		last := metav1.NewTime(now.Add(-c.age))
		if got := phaseForHeartbeat(&last, iv, now); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := phaseForHeartbeat(nil, iv, now); got != "Unreachable" {
		t.Errorf("nil heartbeat: got %q want Unreachable", got)
	}
}

var _ = Describe("FederatedClusterReconciler phase transitions", func() {
	var (
		reconciler *FederatedClusterReconciler
		ctx        context.Context
	)

	const name = "fc-phase-test"
	const intervalSeconds = int32(30)

	BeforeEach(func() {
		ctx = context.Background()
		reconciler = &FederatedClusterReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		fc := &federationv1alpha1.FederatedCluster{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fc)
		if err != nil && errors.IsNotFound(err) {
			fc = &federationv1alpha1.FederatedCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: federationv1alpha1.FederatedClusterSpec{
					HeartbeatIntervalSeconds: intervalSeconds,
				},
			}
			Expect(k8sClient.Create(ctx, fc)).To(Succeed())
		}
	})

	AfterEach(func() {
		fc := &federationv1alpha1.FederatedCluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fc); err == nil {
			_ = k8sClient.Delete(ctx, fc)
		}
	})

	setHeartbeat := func(age time.Duration) {
		fc := &federationv1alpha1.FederatedCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, fc)).To(Succeed())
		ts := metav1.NewTime(time.Now().Add(-age))
		fc.Status.LastHeartbeatTime = &ts
		Expect(k8sClient.Status().Update(ctx, fc)).To(Succeed())
	}

	reconcileAndGet := func() (reconcile.Result, *federationv1alpha1.FederatedCluster) {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name},
		})
		Expect(err).NotTo(HaveOccurred())

		fc := &federationv1alpha1.FederatedCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, fc)).To(Succeed())
		return result, fc
	}

	It("sets Connected for a fresh heartbeat and requeues", func() {
		setHeartbeat(5 * time.Second)

		result, fc := reconcileAndGet()
		Expect(fc.Status.Phase).To(Equal(federationv1alpha1.FederatedClusterConnected))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	It("sets Stale once the heartbeat is older than 3x the interval", func() {
		setHeartbeat(5 * time.Duration(intervalSeconds) * time.Second)

		result, fc := reconcileAndGet()
		Expect(fc.Status.Phase).To(Equal(federationv1alpha1.FederatedClusterStale))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	It("sets Unreachable once the heartbeat is older than 10x the interval", func() {
		setHeartbeat(20 * time.Duration(intervalSeconds) * time.Second)

		result, fc := reconcileAndGet()
		Expect(fc.Status.Phase).To(Equal(federationv1alpha1.FederatedClusterUnreachable))
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})
})

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	llmkubemetrics "github.com/defilantech/llmkube/internal/metrics"
)

// Series counts come from DeletePartialMatch's return value; clearing as we
// count also keeps these specs out of the registry the rest of the suite
// shares. Read any gauge VALUE first — WithLabelValues resurrects a deleted
// series at 0 rather than reporting it missing. The llmkubemetrics.Delete*
// helpers are used where the intent is "clean up this object" rather than
// "count one vec".

func modelLabels(name string) prometheus.Labels {
	return prometheus.Labels{"model": name, "namespace": "default"}
}

func isvcLabels(name string) prometheus.Labels {
	return prometheus.Labels{"inferenceservice": name, "namespace": "default"}
}

var _ = Describe("Operator state metrics lifecycle", func() {
	ctx := context.Background()

	Describe("Model phase gauge", func() {
		var reconciler *ModelReconciler

		BeforeEach(func() {
			reconciler = &ModelReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				StoragePath:          GinkgoT().TempDir(),
				AllowedHostPathRoots: testLocalRoots,
			}
		})

		// Regression: an already-Ready hf:// Model takes the early exit in
		// reconcileRuntimeResolvedSource, so the series used to vanish after
		// the first reconcile and never come back.
		It("stays published across repeated reconciles of an already-Ready model", func() {
			modelName := "model-metrics-steady"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: "hf://org/repo"},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			llmkubemetrics.DeleteModelSeries(modelName, "default")

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
			}

			// First pass drives the Model to Ready.
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			// Clearing here is the point: a second reconcile of an unchanged,
			// already-Ready Model must republish from observed state.
			llmkubemetrics.DeleteModelSeries(modelName, "default")
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.ToFloat64(
				llmkubemetrics.ModelStatus.WithLabelValues(modelName, "default", PhaseReady),
			)).To(Equal(1.0), "second reconcile must republish the Ready series")
			Expect(llmkubemetrics.ModelStatus.DeletePartialMatch(modelLabels(modelName))).To(Equal(1))
		})

		// Real Failed -> Ready sequence: the gauge only accumulates when the
		// controller itself writes the second phase. Deliberately does NOT
		// clear the vec between reconciles — clearing would mask an
		// accumulating gauge.
		It("keeps exactly one series when the phase changes", func() {
			modelName := "model-metrics-transition"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				// Outside testLocalRoots, so the host-path allowlist rejects it
				// and the Model lands in Failed without needing the network.
				Spec: inferencev1alpha1.ModelSpec{Source: "/forbidden/model.gguf"},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			llmkubemetrics.DeleteModelSeries(modelName, "default")

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
			}

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(testutil.ToFloat64(
				llmkubemetrics.ModelStatus.WithLabelValues(modelName, "default", PhaseFailed),
			)).To(Equal(1.0), "rejected source should publish Failed")

			// Point at a source that resolves, so the next reconcile reaches Ready.
			Expect(k8sClient.Get(ctx, req.NamespacedName, model)).To(Succeed())
			model.Spec.Source = "hf://org/repo"
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(testutil.ToFloat64(
				llmkubemetrics.ModelStatus.WithLabelValues(modelName, "default", PhaseReady),
			)).To(Equal(1.0), "the current phase must be the one published")
			Expect(llmkubemetrics.ModelStatus.DeletePartialMatch(modelLabels(modelName))).
				To(Equal(1), "the Failed series must be dropped, not left alongside Ready")
		})

		It("drops the series when the model is gone", func() {
			modelName := "model-metrics-deleted"
			llmkubemetrics.ModelStatus.
				WithLabelValues(modelName, "default", PhaseReady).Set(1)

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(llmkubemetrics.ModelStatus.DeletePartialMatch(modelLabels(modelName))).
				To(Equal(0), "a deleted Model must stop being reported")
		})
	})

	Describe("InferenceService metrics", func() {
		It("keeps exactly one phase series when the phase changes", func() {
			name := "isvc-metrics-transition"
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "some-model"},
			}
			Expect(k8sClient.Create(ctx, isvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, isvc) }()

			llmkubemetrics.DeleteInferenceServiceSeries(name, "default")

			reconciler := &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			// publishInferenceServiceState is what Reconcile's defer calls once
			// the status is written; calling it here drives the same two-phase
			// publish without standing up a full reconcile.
			_, err := reconciler.updateStatusWithSchedulingInfo(
				ctx, isvc, PhaseCreating, false, 0, 1, "", "", nil)
			Expect(err).NotTo(HaveOccurred())
			publishInferenceServiceState(isvc, nil)
			_, err = reconciler.updateStatusWithSchedulingInfo(
				ctx, isvc, PhaseReady, true, 1, 1, "http://example", "", nil)
			Expect(err).NotTo(HaveOccurred())
			publishInferenceServiceState(isvc, nil)

			Expect(testutil.ToFloat64(
				llmkubemetrics.InferenceServicePhase.WithLabelValues(name, "default", PhaseReady),
			)).To(Equal(1.0))
			Expect(llmkubemetrics.InferenceServicePhase.DeletePartialMatch(isvcLabels(name))).
				To(Equal(1), "Creating must be dropped once the service reaches Ready")

			// Replica counts follow the same status update.
			Expect(testutil.ToFloat64(
				llmkubemetrics.InferenceServiceReplicas.WithLabelValues(name, "default", "ready"),
			)).To(Equal(1.0))
			llmkubemetrics.DeleteInferenceServiceSeries(name, "default")
		})

		// The live case: two Ready, serving InferenceServices failed
		// reconcileDeployment on every pass (#1225), returned before the status
		// update, and so exported no series at all for as long as that error
		// persisted. Publishing from the call site cannot cover this; the
		// deferred publish reads what the service already is.
		It("keeps exporting phase, replicas and info when the reconcile errors early", func() {
			name := "isvc-metrics-early-return"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "early-return-model", Namespace: "default"},
				Spec: inferencev1alpha1.ModelSpec{
					Source:   "hf://org/repo",
					Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: acceleratorCUDA},
				},
				Status: inferencev1alpha1.ModelStatus{Phase: PhaseReady},
			}
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name},
				Status: inferencev1alpha1.InferenceServiceStatus{
					Phase:           PhaseReady,
					ReadyReplicas:   2,
					DesiredReplicas: 2,
				},
			}

			reconciler := &InferenceServiceReconciler{Scheme: k8sClient.Scheme()}
			// Seeding the Deployment the controller would build puts
			// reconcileDeployment on its in-place Update path, which is where the
			// live failure lands; the interceptor then rejects that Update.
			deployment := reconciler.constructDeployment(isvc, model, 2)
			reconciler.Client = fake.NewClientBuilder().
				WithScheme(k8sClient.Scheme()).
				WithObjects(model, isvc, deployment).
				WithStatusSubresource(&inferencev1alpha1.InferenceService{}, &inferencev1alpha1.Model{}).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object,
						opts ...client.UpdateOption) error {
						if _, ok := obj.(*appsv1.Deployment); ok {
							return fmt.Errorf("activeDeadlineSeconds in ReplicaSet is not Supported")
						}
						return c.Update(ctx, obj, opts...)
					},
				}).
				Build()

			llmkubemetrics.DeleteInferenceServiceSeries(name, "default")

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
			Expect(err).To(HaveOccurred(), "this spec is only meaningful if the reconcile fails")

			Expect(testutil.ToFloat64(
				llmkubemetrics.InferenceServicePhase.WithLabelValues(name, "default", PhaseReady),
			)).To(Equal(1.0), "a failed reconcile must still report the stored phase")
			Expect(testutil.ToFloat64(
				llmkubemetrics.InferenceServiceReplicas.WithLabelValues(name, "default", "ready"),
			)).To(Equal(2.0))
			Expect(testutil.ToFloat64(
				llmkubemetrics.InferenceServiceInfo.WithLabelValues(name, "default", acceleratorCUDA, "llamacpp"),
			)).To(Equal(1.0), "info must carry the Model's accelerator, not the cpu fallback")
			llmkubemetrics.DeleteInferenceServiceSeries(name, "default")
		})

		It("drops phase, info and replica series when the service is gone", func() {
			name := "isvc-metrics-deleted"
			llmkubemetrics.InferenceServicePhase.
				WithLabelValues(name, "default", PhaseReady).Set(1)
			llmkubemetrics.InferenceServiceInfo.
				WithLabelValues(name, "default", "cpu", "llamacpp").Set(1)
			llmkubemetrics.InferenceServiceReplicas.
				WithLabelValues(name, "default", "ready").Set(1)

			reconciler := &InferenceServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Every per-service vec must be covered, or a deleted service keeps
			// reporting through whichever one was missed.
			Expect(llmkubemetrics.InferenceServicePhase.DeletePartialMatch(isvcLabels(name))).To(Equal(0))
			Expect(llmkubemetrics.InferenceServiceInfo.DeletePartialMatch(isvcLabels(name))).To(Equal(0))
			Expect(llmkubemetrics.InferenceServiceReplicas.DeletePartialMatch(isvcLabels(name))).To(Equal(0))
		})
	})
})

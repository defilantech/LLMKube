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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var _ = Describe("Model source revalidation", func() {
	ctx := context.Background()

	// headServer returns an httptest server whose HEAD responses advertise the
	// given ETag and Content-Length. The etag/length are read at request time
	// from the pointers so a test can mutate them between reconciles to
	// simulate an upstream re-upload.
	newHeadServer := func(etag *string, length *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", *etag)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", *length))
			w.WriteHeader(http.StatusOK)
		}))
	}

	Context("IfNotPresent policy (default)", func() {
		It("flags SourceDrifted when the upstream ETag changes but does not re-download", func() {
			etag := `"v1"`
			length := 100
			server := newHeadServer(&etag, &length)
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-ifnotpresent"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				// RefreshPolicy omitted -> defaults to IfNotPresent via the CRD.
				Spec: inferencev1alpha1.ModelSpec{Source: source, RefreshPolicy: RefreshPolicyIfNotPresent},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			// Zero interval so every reconcile revalidates.
			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}

			// First reconcile marks Ready (runtime-resolved). Second reconcile
			// records the baseline fingerprint.
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			baseline := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, baseline)).To(Succeed())
			Expect(baseline.Status.SourceETag).To(Equal(`"v1"`))

			// Upstream re-uploads with a new ETag.
			etag = `"v2"`
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(sourceDriftedStatus(updated)).To(Equal(metav1.ConditionTrue),
				"IfNotPresent must surface drift via the SourceDrifted condition")
			// Controller-side storage stays empty: HTTP sources are workload-resolved,
			// so the controller does not (re-)download under any policy.
			entries, _ := os.ReadDir(tempDir)
			Expect(entries).To(BeEmpty(), "controller must not download HTTP sources")
		})
	})

	// The controller never downloads a remote source: the serving pod's
	// model-downloader init container does, after the Model is already Ready.
	// The drift check compares the upstream with the fingerprint the
	// controller recorded, so its condition must not claim a cached copy that
	// may not exist yet (#2415).
	Context("workload-resolved remote source", func() {
		It("reports the recorded fingerprint, not a cached copy", func() {
			etag := `"v1"`
			length := 100
			server := newHeadServer(&etag, &length)
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-workload-honest"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source, RefreshPolicy: RefreshPolicyIfNotPresent},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}

			// Ready first, then the baseline: the order a first deploy sees,
			// with the pod's download still to come.
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			baselined := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, baselined)).To(Succeed())
			Expect(baselined.Status.Phase).To(Equal(PhaseReady), "the Model must still go Ready before the download")
			Expect(baselined.Status.Path).To(BeEmpty())
			inSync := sourceDriftedCondition(baselined)
			Expect(inSync).NotTo(BeNil())
			Expect(inSync.Status).To(Equal(metav1.ConditionFalse))
			Expect(inSync.Reason).To(Equal("UpstreamUnchanged"))
			Expect(inSync.Message).To(ContainSubstring("does not cache"))
			Expect(inSync.Message).NotTo(ContainSubstring("Cached model"))

			// An upstream re-upload is drift against the recorded fingerprint;
			// there is no controller-side copy for it to differ from.
			etag = `"v2"`
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			drifted := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, drifted)).To(Succeed())
			changed := sourceDriftedCondition(drifted)
			Expect(changed).NotTo(BeNil())
			Expect(changed.Status).To(Equal(metav1.ConditionTrue))
			Expect(changed.Reason).To(Equal("UpstreamChanged"))
			Expect(changed.Message).To(ContainSubstring("fingerprint"))
			Expect(changed.Message).NotTo(ContainSubstring("cached copy"))
		})

		It("rewrites a cached-copy claim an older operator wrote", func() {
			etag := `"v1"`
			length := 100
			server := newHeadServer(&etag, &length)
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-workload-upgrade"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			// The status an older operator left: Ready, baselined, and
			// SourceDrifted=False claiming a cached copy.
			transitioned := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
			model.Status.Phase = PhaseReady
			model.Status.CacheKey = computeCacheKey(source)
			model.Status.SourceETag = `"v1"`
			model.Status.SourceContentLength = 100
			model.Status.Conditions = []metav1.Condition{{
				Type: ConditionSourceDrifted, Status: metav1.ConditionFalse,
				Reason: "InSync", Message: "Cached model matches the upstream source",
				LastTransitionTime: transitioned,
			}}
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			cond := sourceDriftedCondition(updated)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("UpstreamUnchanged"))
			Expect(cond.Message).NotTo(ContainSubstring("Cached model"))
			// The status did not change, so neither does the transition time.
			Expect(cond.LastTransitionTime.Time).To(BeTemporally("==", transitioned.Time))
		})
	})

	Context("OnChange policy on a controller-managed local source", func() {
		It("re-downloads (overwrites) when the upstream file changes and clears drift", func() {
			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			srcDir, err := os.MkdirTemp("", "llmkube-src-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(srcDir) }()
			srcFile := filepath.Join(srcDir, "src.gguf")
			Expect(os.WriteFile(srcFile, []byte("original-bytes"), 0644)).To(Succeed())
			source := fmt.Sprintf("file://%s", srcFile)

			modelName := "revalidate-onchange-local"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source, RefreshPolicy: RefreshPolicyOnChange},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}

			// First reconcile copies the model into the cache and baselines.
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			first := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, first)).To(Succeed())
			Expect(first.Status.Phase).To(Equal(PhaseReady))
			cachedPath := first.Status.Path
			Expect(cachedPath).NotTo(BeEmpty())
			data, err := os.ReadFile(cachedPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("original-bytes"))

			// Upstream file changes (larger content => different size/mtime).
			Expect(os.WriteFile(srcFile, []byte("brand-new-corrected-bytes"), 0644)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			data, err = os.ReadFile(updated.Status.Path)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal("brand-new-corrected-bytes"),
				"OnChange must overwrite the cached file with the new upstream bytes")
			Expect(sourceDriftedStatus(updated)).To(Equal(metav1.ConditionFalse),
				"drift must clear after a successful re-download")
			// The controller holds this copy itself, so it may say it matches.
			Expect(sourceDriftedCondition(updated).Reason).To(Equal("InSync"))
			Expect(sourceDriftedCondition(updated).Message).To(Equal("Cached model matches the upstream source"))
		})
	})

	Context("backward compatibility", func() {
		It("records a baseline on first revalidation without re-downloading", func() {
			etag := `"baseline"`
			length := 42
			server := newHeadServer(&etag, &length)
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-baseline"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source, RefreshPolicy: RefreshPolicyOnChange},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			// Simulate an existing cached model upgraded from an older operator:
			// Ready, no stored fingerprint.
			model.Status.Phase = PhaseReady
			model.Status.CacheKey = computeCacheKey(source)
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}

			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.Status.SourceETag).To(Equal(`"baseline"`), "first revalidation must record the baseline")
			Expect(sourceDriftedStatus(updated)).NotTo(Equal(metav1.ConditionTrue),
				"baseline must not be treated as drift")
			entries, _ := os.ReadDir(tempDir)
			Expect(entries).To(BeEmpty(), "baseline recording must not trigger a download")
		})
	})

	Context("robustness", func() {
		It("keeps the cache and flags no drift when the HEAD probe fails (5xx)", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-5xx"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source, RefreshPolicy: RefreshPolicyOnChange},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			// Pre-existing baseline fingerprint that the failing probe cannot refute.
			model.Status.Phase = PhaseReady
			model.Status.CacheKey = computeCacheKey(source)
			model.Status.SourceETag = `"known-good"`
			model.Status.SourceContentLength = 1234
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Nanosecond,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred(), "a failed probe must not surface an error")

			updated := &inferencev1alpha1.Model{}
			Expect(k8sClient.Get(ctx, req.NamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(PhaseReady), "phase must stay Ready on an indeterminate probe")
			Expect(sourceDriftedStatus(updated)).NotTo(Equal(metav1.ConditionTrue),
				"an indeterminate probe must not flag drift")
			Expect(updated.Status.SourceETag).To(Equal(`"known-good"`), "baseline must be preserved")
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "should still requeue for the next attempt")
		})
	})

	Context("cadence gating", func() {
		It("skips revalidation when within the interval", func() {
			reconciler := &ModelReconciler{RevalidateInterval: time.Hour}
			recent := metav1.NewTime(time.Now().Add(-time.Minute))
			model := &inferencev1alpha1.Model{
				Status: inferencev1alpha1.ModelStatus{LastRevalidated: &recent},
			}
			Expect(reconciler.shouldRevalidate(model, time.Now())).To(BeFalse(),
				"must not revalidate again within the interval")

			stale := metav1.NewTime(time.Now().Add(-2 * time.Hour))
			model.Status.LastRevalidated = &stale
			Expect(reconciler.shouldRevalidate(model, time.Now())).To(BeTrue(),
				"must revalidate once the interval has elapsed")
		})

		It("does not probe the upstream when the cadence gate is closed", func() {
			var hits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set("ETag", `"x"`)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			tempDir, err := os.MkdirTemp("", "llmkube-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tempDir) }()

			source := server.URL + "/model.gguf"
			modelName := "revalidate-cadence"
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelName, Namespace: "default"},
				Spec:       inferencev1alpha1.ModelSpec{Source: source},
			}
			Expect(k8sClient.Create(ctx, model)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, model) }()

			// Already revalidated moments ago, long interval => gate closed.
			now := metav1.Now()
			model.Status.Phase = PhaseReady
			model.Status.CacheKey = computeCacheKey(source)
			model.Status.LastRevalidated = &now
			model.Status.SourceETag = `"x"`
			Expect(k8sClient.Status().Update(ctx, model)).To(Succeed())

			reconciler := &ModelReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				StoragePath: tempDir, RevalidateInterval: time.Hour,
				AllowedHostPathRoots: testLocalRoots,
				AllowedRemoteHosts:   testRemoteHosts,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: modelName, Namespace: "default"}}
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(hits).To(Equal(0), "cadence gate must suppress the HEAD probe within the interval")
		})
	})
})

func sourceDriftedStatus(model *inferencev1alpha1.Model) metav1.ConditionStatus {
	for _, c := range model.Status.Conditions {
		if c.Type == ConditionSourceDrifted {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}

func sourceDriftedCondition(model *inferencev1alpha1.Model) *metav1.Condition {
	for i := range model.Status.Conditions {
		if model.Status.Conditions[i].Type == ConditionSourceDrifted {
			return &model.Status.Conditions[i]
		}
	}
	return nil
}

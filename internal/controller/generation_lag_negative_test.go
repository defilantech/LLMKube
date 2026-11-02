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
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/watchcanary"
)

// The unreconciled-generation alert (#2308) counts an object whose
// metadata.generation is ahead of every condition's observedGeneration. These
// tests run the real reconcilers through the long-running paths the story
// names and assert, on the stored object, that a pass the controller did run
// leaves the object counted as reconciled. A path that holds work back
// without stamping the current generation would page on a healthy cluster.

func assertGenerationReconciled(t *testing.T, c client.Client, obj client.Object, conditions func() []metav1.Condition, what string) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatalf("get %s: %v", obj.GetName(), err)
	}
	conds := conditions()
	if watchcanary.GenerationUnreconciled(obj, conds) {
		t.Fatalf("%s: generation %d is ahead of every condition (max observedGeneration %d); the alert would count a reconciled object. conditions: %+v",
			what, obj.GetGeneration(), watchcanary.MaxObservedGeneration(conds), conds)
	}
}

func readyModelFor(t *testing.T, c client.Client, name string) *inferencev1alpha1.Model {
	t.Helper()
	ctx := context.Background()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/model.gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "cpu"},
		},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	model.Status.Phase = PhaseReady
	if err := c.Status().Update(ctx, model); err != nil {
		t.Fatalf("mark model ready: %v", err)
	}
	return model
}

func bumpISVCImage(t *testing.T, c client.Client, key types.NamespacedName, image string) {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := c.Get(context.Background(), key, isvc); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	isvc.Spec.Image = image
	if err := c.Update(context.Background(), isvc); err != nil {
		t.Fatalf("update isvc image: %v", err)
	}
	if isvc.Generation < 2 {
		t.Fatalf("fixture: the image change did not bump generation (got %d)", isvc.Generation)
	}
}

// A spec change that rolls out normally: the reconcile that applies it stamps
// the new generation.
func TestGenerationLagNotCountedDuringNormalRollout(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	model := readyModelFor(t, c, "genlag-rollout-model")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "genlag-rollout", Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: ptrInt32(1),
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
		},
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	key := client.ObjectKeyFromObject(isvc)
	r := &InferenceServiceReconciler{Client: c, Scheme: c.Scheme(), InitContainerImage: "docker.io/curlimages/curl:8.18.0"}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	assertGenerationReconciled(t, c, isvc, func() []metav1.Condition { return isvc.Status.Conditions }, "created InferenceService")

	bumpISVCImage(t, c, key, "ghcr.io/ggml-org/llama.cpp:server-v2")
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("rollout reconcile: %v", err)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/ggml-org/llama.cpp:server-v2" {
		t.Fatalf("fixture: the rollout did not apply the new image, got %q", got)
	}
	assertGenerationReconciled(t, c, isvc, func() []metav1.Condition { return isvc.Status.Conditions }, "InferenceService mid-rollout")
}

// A spec change held back by drain-before-rollout (waitForIdle with busy
// slots): the Deployment keeps the old template, possibly for the whole idle
// timeout, and the object must still read as reconciled.
func TestGenerationLagNotCountedDuringDrainBeforeRolloutWait(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	model := readyModelFor(t, c, "genlag-drain-model")

	var busy atomic.Bool
	slots := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slots" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if busy.Load() {
			_, _ = w.Write([]byte(`[{"id":0,"is_processing":true}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":0,"is_processing":false}]`))
	}))
	defer slots.Close()

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "genlag-drain", Namespace: testNS},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: model.Name,
			Replicas: ptrInt32(1),
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
			RolloutPolicy: &inferencev1alpha1.RolloutPolicySpec{
				WaitForIdle:        true,
				IdleTimeoutSeconds: 3600,
			},
		},
	}
	if err := c.Create(ctx, isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	key := client.ObjectKeyFromObject(isvc)
	r := &InferenceServiceReconciler{
		Client:             c,
		Scheme:             c.Scheme(),
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		RolloutIdleBaseURL: slots.URL,
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	busy.Store(true)
	bumpISVCImage(t, c, key, "ghcr.io/ggml-org/llama.cpp:server-v2")
	// Two passes: the deferral is re-evaluated on every requeue while the
	// slots stay busy, and neither may leave the generation unstamped.
	for pass := 1; pass <= 2; pass++ {
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("deferred reconcile %d: %v", pass, err)
		}
		if result.RequeueAfter <= 0 {
			t.Fatalf("fixture: deferred reconcile %d did not requeue", pass)
		}
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, key, dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/ggml-org/llama.cpp:server" {
		t.Fatalf("fixture: the rollout was not held back, deployment image %q", got)
	}
	if err := c.Get(ctx, key, isvc); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	if cond := meta.FindStatusCondition(isvc.Status.Conditions, ConditionRolloutDeferred); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("fixture: expected RolloutDeferred=True, got %+v", cond)
	}
	assertGenerationReconciled(t, c, isvc, func() []metav1.Condition { return isvc.Status.Conditions }, "InferenceService waiting to drain")
}

// A Model downloading through its prefetch Job: the Job can run for hours on
// a large model. Neither the first download nor a spec change that lands
// while the download is still running may read as unreconciled.
func TestGenerationLagNotCountedDuringModelDownload(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "genlag-download", Namespace: testNS},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/models/llama.gguf",
			Prefetch: true,
		},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	key := client.ObjectKeyFromObject(model)
	r := &ModelReconciler{
		Client:               c,
		Scheme:               c.Scheme(),
		InitContainerImage:   "docker.io/curlimages/curl:8.18.0",
		DefaultFSGroup:       102,
		ModelCacheSize:       "10Gi",
		ModelCacheAccessMode: "ReadWriteOnce",
		ModelCacheMode:       ModelCacheModeShared,
		ModelCachePath:       "/models",
	}
	reconcileDownloading := func(what string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Fatalf("%s reconcile: %v", what, err)
		}
		if err := c.Get(ctx, key, model); err != nil {
			t.Fatalf("get model: %v", err)
		}
		if model.Status.Phase != PhaseDownloading {
			t.Fatalf("fixture: %s: expected the Model Downloading, got %q", what, model.Status.Phase)
		}
		assertGenerationReconciled(t, c, model, func() []metav1.Condition { return model.Status.Conditions }, what)
	}

	reconcileDownloading("first download")
	// The Job is still running; a later poll changes nothing.
	reconcileDownloading("download poll")

	// A spec change that leaves the download running (quantization is a
	// label for consumers, not part of what the Job fetches).
	model.Spec.Quantization = "Q8_0"
	if err := c.Update(ctx, model); err != nil {
		t.Fatalf("update model: %v", err)
	}
	if model.Generation < 2 {
		t.Fatalf("fixture: the spec change did not bump generation (got %d)", model.Generation)
	}
	reconcileDownloading("spec change mid-download")
}

// A Ready Model whose spec changes in a field that leaves the source alone
// (quantization, hardware): the source-keyed early exits serve the existing
// resolution, and the pass that does so must still read as reconciled at the
// new generation, or the alert counts a healthy Model forever.
func TestGenerationLagNotCountedForReadyModelSpecEdit(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()

	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "model.gguf")
	if err := os.WriteFile(srcFile, []byte("not really gguf"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "genlag-weights", Namespace: testNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := c.Create(ctx, pvc); err != nil {
		t.Fatalf("create pvc: %v", err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, pvc); err != nil {
		t.Fatalf("bind pvc: %v", err)
	}

	cases := []struct {
		name   string
		source string
	}{
		{"local file served from the cache", "file://" + srcFile},
		{"pvc source", "pvc://genlag-weights/model.gguf"},
		{"oci source", "oci://registry.example.com/models/genlag:1"},
		{"remote source resolved at runtime", "https://example.com/models/genlag.gguf"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("genlag-ready-%d", i), Namespace: testNS},
				Spec:       inferencev1alpha1.ModelSpec{Source: tc.source},
			}
			if err := c.Create(ctx, model); err != nil {
				t.Fatalf("create model: %v", err)
			}
			key := client.ObjectKeyFromObject(model)
			r := &ModelReconciler{
				Client:               c,
				Scheme:               c.Scheme(),
				StoragePath:          t.TempDir(),
				AllowedHostPathRoots: testLocalRoots,
			}
			reconcileReady := func(what string) {
				t.Helper()
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Fatalf("%s reconcile: %v", what, err)
				}
				if err := c.Get(ctx, key, model); err != nil {
					t.Fatalf("get model: %v", err)
				}
				if model.Status.Phase != PhaseReady {
					t.Fatalf("fixture: %s: expected Ready, got %q (conditions %+v)", what, model.Status.Phase, model.Status.Conditions)
				}
				assertGenerationReconciled(t, c, model, func() []metav1.Condition { return model.Status.Conditions }, what)
			}

			reconcileReady("first")
			// Ready and current: a later pass takes the early exit.
			reconcileReady("steady state")

			model.Spec.Quantization = "Q8_0"
			if err := c.Update(ctx, model); err != nil {
				t.Fatalf("update model: %v", err)
			}
			if model.Generation < 2 {
				t.Fatalf("fixture: the spec change did not bump generation (got %d)", model.Generation)
			}
			reconcileReady("non-source spec edit")
		})
	}
}

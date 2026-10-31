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
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/ocipublish"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func ociPrefetchFixture(t *testing.T) (*ModelReconciler, *inferencev1alpha1.Model) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, inferencev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	model := &inferencev1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "weights", Namespace: "test", UID: types.UID("model-uid"), Generation: 1},
		Spec: inferencev1alpha1.ModelSpec{Source: "https://example.com/weights.gguf", Prefetch: true,
			PrefetchOCI: &inferencev1alpha1.OCIPrefetchSpec{Image: "registry.example.com/models/test:v1", PushSecretRef: &corev1.LocalObjectReference{Name: "registry"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(model, &batchv1.Job{}, &corev1.Pod{}).WithObjects(model).Build()
	return &ModelReconciler{Client: c, Scheme: scheme, OCIPublisherImage: "operator:test", InitContainerImage: defaultPrefetchImage}, model
}

// The OCI dispatch and emptyDir builder are the edits this test protects:
// removing either must produce no publishing Job or a PVC-backed Job.
func TestOCIPrefetchJobAndLifecycle(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	ctx := context.Background()
	server := httptest.NewServer(registry.New())
	defer server.Close()
	model.Spec.PrefetchOCI.Image = strings.TrimPrefix(server.URL, "http://") + "/models/test:v1"
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("want one OCI publishing Job, got %d", len(jobs.Items))
	}
	job := &jobs.Items[0]
	assertOCIPublishingJob(t, r, model, job)
	publisher := job.Spec.Template.Spec.Containers[0]
	if model.Status.Phase == PhaseReady {
		t.Fatal("Model ready before push")
	}
	// A completed Job alone is insufficient: the publisher must return a digest.
	job.UID = "job-uid"
	if err := r.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if model.Status.Phase == PhaseReady {
		t.Fatal("Model ready without publisher result")
	}
	// Feed the real publisher's result bytes to the controller, not a result
	// serialized with a consumer-side type.
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "test-weights.gguf"), []byte("weights"), 0600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(directory, "result")
	args := append(append([]string(nil), publisher.Args...), "--directory="+directory, "--result="+resultPath)
	if err := ocipublish.Run(ctx, args); err != nil {
		t.Fatal(err)
	}
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := string(resultBytes)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: model.Namespace, Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: ociPublisherContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: digest}}}}},
	}
	if err := r.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if model.Status.Phase == PhaseReady {
		t.Fatal("accepted digest from an unowned Pod")
	}
	if err := controllerutil.SetControllerReference(job, pod, r.Scheme); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if model.Status.Phase != PhaseReady || model.Status.PrefetchOCI.Image != digest {
		t.Fatalf("publication status: %+v", model.Status.PrefetchOCI)
	}
	if err := r.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("recreated completed Job after TTL cleanup")
	}
	// An edited destination must not reuse an earlier successful publication.
	model.Spec.PrefetchOCI.Image = "registry.example.com/models/test:v2"
	model.Generation++
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if model.Status.PrefetchOCI.Image != "" || model.Status.Phase == PhaseReady {
		t.Fatal("reused stale publication")
	}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Spec.Template.Spec.Containers[0].Args[0] != "--image=registry.example.com/models/test:v2" {
		t.Fatal("destination edit did not create a new publisher")
	}
}

func TestOCIPrefetchFileContract(t *testing.T) {
	for _, multi := range []bool{false, true} {
		r, model := ociPrefetchFixture(t)
		if multi {
			model.Spec.Files = []string{"shards/first.gguf", "shards/second.gguf"}
			model.Spec.Mmproj = "projector.gguf"
		}
		job, files, err := r.buildOCIPrefetchJob(context.Background(), model, "fingerprint")
		if err != nil {
			t.Fatal(err)
		}
		var mappings []ocipublish.File
		for _, arg := range job.Spec.Template.Spec.Containers[0].Args {
			if strings.HasPrefix(arg, "--files=") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "--files=")), &mappings); err != nil {
					t.Fatal(err)
				}
			}
		}
		want := []ocipublish.File{{Path: "test-weights.gguf", Name: "weights.gguf"}}
		if multi {
			want = []ocipublish.File{{Path: "test-weights/shards/first.gguf", Name: "shards/first.gguf"}, {Path: "test-weights/shards/second.gguf", Name: "shards/second.gguf"}, {Path: "test-weights/projector.gguf", Name: "projector.gguf"}}
		}
		if !reflect.DeepEqual(mappings, want) {
			t.Fatalf("publisher mappings=%+v want=%+v", mappings, want)
		}
		consumer := model.DeepCopy()
		consumer.Name = "promoted-model"
		consumer.Spec.Source = "oci://registry.example.com/models/test:v1"
		consumer.Spec.Files = files
		consumer.Spec.PrefetchOCI = nil
		storage := buildOCIStorageConfig(consumer, defaultPrefetchImage)
		if storage.modelPath != "/model-source/"+mappings[0].Name {
			t.Fatalf("consumer path=%s, published primary=%s", storage.modelPath, mappings[0].Name)
		}
		if len(storage.initContainers) != 0 {
			t.Fatal("OCI consumer downloads from provider")
		}
	}
}

func TestOCIPrefetchAdmission(t *testing.T) {
	c, _, _ := sharedTestEnv(t, "../../config/crd/bases")
	for _, kind := range []string{"valid", "disabled", "cache"} {
		_, model := ociPrefetchFixture(t)
		model.Namespace, model.Name, model.UID, model.ResourceVersion = testNS, "oci-"+kind, "", ""
		model.Spec.Format = "gguf"
		if kind == "disabled" {
			model.Spec.Prefetch = false
		}
		if kind == "cache" {
			model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache"}
		}
		err := c.Create(context.Background(), model)
		if (kind == "valid") != (err == nil) {
			t.Fatalf("%s admission: %v", kind, err)
		}
	}
}

func TestOCIPrefetchFailuresAndStaleJobs(t *testing.T) {
	for _, scenario := range []string{"failed", "source-change", "destination-change", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			r, model := ociPrefetchFixture(t)
			ctx := context.Background()
			if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
				t.Fatal(err)
			}
			jobs := &batchv1.JobList{}
			if err := r.List(ctx, jobs); err != nil {
				t.Fatal(err)
			}
			job := &jobs.Items[0]
			switch scenario {
			case "failed":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			case "source-change":
				model.Spec.Source = "https://example.com/new.gguf"
			case "destination-change":
				model.Spec.PrefetchOCI.Image = "registry.example.com/models/new:v2"
			case "disabled":
				model.Spec.PrefetchOCI = nil
				model.Spec.Prefetch = false
			}
			if err := r.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := r.Update(ctx, model); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
				t.Fatal(err)
			}
			if scenario == "failed" {
				if model.Status.Phase != PhaseFailed || model.Status.PrefetchOCI.Image != "" {
					t.Fatalf("failed publication: %+v", model.Status)
				}
			} else {
				if err := r.List(ctx, jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) > 0 && jobs.Items[0].DeletionTimestamp.IsZero() {
					t.Fatal("obsolete publishing Job was not deleted")
				}
				if model.Status.PrefetchOCI != nil && model.Status.PrefetchOCI.Image != "" {
					t.Fatal("stale publication retained")
				}
			}
		})
	}
}

func TestOCIPrefetchPreservesServicePVCStaging(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	r.ModelCachePath = "/models"
	r.ModelCacheMode = ModelCacheModeShared
	ctx := context.Background()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "service-cache", Namespace: model.Namespace}}
	service := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: model.Namespace},
		Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: model.Name, ModelCache: &inferencev1alpha1.ModelCacheSpec{Prefetch: true, ClaimName: claim.Name}},
	}
	if err := r.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, service); err != nil {
		t.Fatal(err)
	}
	// The normal source path seeds this key for service-requested staging.
	seedPrefetchCacheKey(model)
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("want independent PVC and OCI Jobs, got %d", len(jobs.Items))
	}
	found := false
	for _, job := range jobs.Items {
		for _, volume := range job.Spec.Template.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claim.Name {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("service prefetch lost its cache claim")
	}
}

func assertOCIPublishingJob(t *testing.T, r *ModelReconciler, model *inferencev1alpha1.Model, job *batchv1.Job) {
	t.Helper()
	ctx := context.Background()
	podSpec := job.Spec.Template.Spec
	if !metav1.IsControlledBy(job, model) {
		t.Fatal("Job is not owned by Model")
	}
	for _, volume := range podSpec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			t.Fatal("OCI publication provisioned a PVC mount")
		}
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims); err != nil || len(claims.Items) != 0 {
		t.Fatalf("claims=%v err=%v", claims.Items, err)
	}
	publisher := podSpec.Containers[0]
	if publisher.Name != ociPublisherContainer || publisher.Image != "operator:test" {
		t.Fatalf("publisher: %+v", publisher)
	}
	for _, c := range podSpec.InitContainers {
		for _, mount := range c.VolumeMounts {
			if mount.Name == "registry-auth" {
				t.Fatal("downloader received push credentials")
			}
		}
	}
	foundAuth := false
	for _, mount := range publisher.VolumeMounts {
		if mount.Name == "registry-auth" {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatal("publisher has no credentials mount")
	}
}

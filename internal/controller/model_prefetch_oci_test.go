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
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/ocipublish"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
			PrefetchOCI: &inferencev1alpha1.OCIPrefetchSpec{EphemeralStorage: "2Gi", Image: "registry.example.com/models/test:v1", PushSecretRef: &corev1.LocalObjectReference{Name: "registry"}},
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
	for _, kind := range []string{"valid", "implicit", "cache", "missing-budget", "zero-budget", "negative-budget", "invalid-budget"} {
		_, model := ociPrefetchFixture(t)
		model.Namespace, model.Name, model.UID, model.ResourceVersion = testNS, "oci-"+kind, "", ""
		model.Spec.Format = "gguf"
		if kind == "implicit" {
			model.Spec.Prefetch = false
		}
		if kind == "cache" {
			model.Spec.ModelCache = &inferencev1alpha1.ModelCacheSpec{ClaimName: "cache"}
		}
		switch kind {
		case "missing-budget":
			model.Spec.PrefetchOCI.EphemeralStorage = ""
		case "zero-budget":
			model.Spec.PrefetchOCI.EphemeralStorage = "0"
		case "negative-budget":
			model.Spec.PrefetchOCI.EphemeralStorage = "-1Gi"
		case "invalid-budget":
			model.Spec.PrefetchOCI.EphemeralStorage = "bad"
		}
		err := c.Create(context.Background(), model)
		if (kind == "valid" || kind == "implicit") != (err == nil) {
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
				if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 86400 {
					t.Fatal("failed publisher has no 24-hour cleanup")
				}
				if err := r.Delete(ctx, job); err != nil {
					t.Fatal(err)
				}
				if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
					t.Fatal(err)
				}
				if err := r.List(ctx, jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 1 {
					t.Fatal("failed publisher did not retry after cleanup")
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

func TestOCIPrefetchStorageBudget(t *testing.T) {
	for _, size := range []string{"2Gi", "150Gi", "", "0", "-1Gi", "bad"} {
		t.Run(size, func(t *testing.T) {
			r, model := ociPrefetchFixture(t)
			model.Spec.PrefetchOCI.EphemeralStorage = size
			job, _, err := r.buildOCIPrefetchJob(context.Background(), model, "fingerprint")
			want, parseErr := resource.ParseQuantity(size)
			if parseErr != nil || want.Sign() <= 0 {
				if err == nil {
					t.Fatalf("created a Job with invalid ephemeralStorage %q", size)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pod := job.Spec.Template.Spec
			for _, container := range append(pod.InitContainers, pod.Containers...) {
				for _, resources := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
					got := resources[corev1.ResourceEphemeralStorage]
					if got.Cmp(want) != 0 {
						t.Errorf("%s storage budget = %s, want %s", container.Name, got.String(), size)
					}
				}
			}
			for _, volume := range pod.Volumes {
				if volume.Name == "model-storage" {
					if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(want) != 0 {
						t.Fatalf("model-storage has no matching emptyDir sizeLimit: %+v", volume)
					}
					return
				}
			}
			t.Fatal("Job has no model-storage volume")
		})
	}
}

func TestOCIPrefetchRejectsMissingStorageBudget(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	model.Spec.PrefetchOCI.EphemeralStorage = ""
	ctx := context.Background()
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("created a publishing Job without a disk budget")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatal(err)
	}
	if model.Status.Phase != PhaseFailed || model.Status.PrefetchOCI == nil ||
		!strings.Contains(model.Status.PrefetchOCI.Message, "ephemeralStorage must be a positive") {
		t.Fatalf("missing storage budget was not reported: %+v", model.Status)
	}
}

// Removing the prefetch boolean guard must allow an OCI Job to be created.
func TestOCIPrefetchImplicit(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	model.Spec.Prefetch = false
	ctx := context.Background()
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("want one implicit OCI publishing Job, got %d", len(jobs.Items))
	}
	assertOCIPublishingJob(t, r, model, &jobs.Items[0])
}

// The resolved-file limit must stop oversized Jobs before any download starts.
func TestOCIPrefetchLayerLimit(t *testing.T) {
	for _, tc := range []struct {
		count     int
		projector bool
		allowed   bool
	}{
		{64, false, true}, {65, false, false}, {63, true, true}, {64, true, false},
	} {
		t.Run(fmt.Sprintf("files=%d/projector=%t", tc.count, tc.projector), func(t *testing.T) {
			r, model := ociPrefetchFixture(t)
			for i := 0; i < tc.count; i++ {
				model.Spec.Files = append(model.Spec.Files, fmt.Sprintf("shard-%03d.gguf", i))
			}
			if tc.projector {
				model.Spec.Mmproj = "projector.gguf"
			}
			ctx := context.Background()
			if err := r.Update(ctx, model); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
				t.Fatal(err)
			}
			jobs := &batchv1.JobList{}
			if err := r.List(ctx, jobs); err != nil {
				t.Fatal(err)
			}
			if tc.allowed {
				if len(jobs.Items) != 1 {
					t.Fatalf("want one publishing Job, got %d", len(jobs.Items))
				}
			} else {
				if len(jobs.Items) != 0 {
					t.Fatal("created publishing Job exceeding 64 layers")
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
					t.Fatal(err)
				}
				if model.Status.Phase != PhaseFailed || !strings.Contains(model.Status.PrefetchOCI.Message, "at most 64 files") {
					t.Fatalf("layer limit not reported: %+v", model.Status)
				}
			}
		})
	}
}

func TestOCIPrefetchLayerLimitAfterGlobExpansion(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	r.AllowedRemoteHosts = []string{"127.0.0.1"}
	var entries []string
	for i := 0; i < 65; i++ {
		entries = append(entries, fmt.Sprintf(`{"type":"file","path":"shard-%03d.safetensors","size":1}`, i))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "["+strings.Join(entries, ",")+"]")
	}))
	defer server.Close()
	ctx := context.Background()
	model.Spec.Source = "hf://org/model"
	model.Spec.Format = "safetensors"
	model.Spec.Files = []string{"shard-000.safetensors", "*.safetensors"}
	model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "source"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: model.Namespace}, Data: map[string][]byte{"HF_ENDPOINT": []byte(server.URL)}}
	if err := r.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("created publishing Job exceeding 64 expanded files")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatal(err)
	}
	if model.Status.PrefetchOCI == nil || !strings.Contains(model.Status.PrefetchOCI.Message, "at most 64 files") {
		t.Fatalf("expanded layer limit not reported: %+v", model.Status)
	}
}

// Re-stamping OCI conditions or setting Running before reading a terminal Job
// must not turn a steady publication into another status write.
func TestOCIPrefetchStableStatus(t *testing.T) {
	for _, phase := range []string{"Running", "Succeeded", "Failed", "missing-digest", "invalid-spec"} {
		t.Run(phase, func(t *testing.T) {
			r, model := ociPrefetchFixture(t)
			ctx := context.Background()
			c, _, _ := sharedTestEnv(t, "../../config/crd/bases")
			r.Client = c
			model.Namespace, model.UID, model.ResourceVersion = testNS, "", ""
			if err := r.Create(ctx, model); err != nil {
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
			job := &batchv1.Job{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: model.Namespace, Name: model.Status.PrefetchOCI.JobName}, job); err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "Succeeded":
				model.Status.PrefetchOCI.Phase = phase
				model.Status.PrefetchOCI.Image = "oci://registry.example.com/models/test@sha256:" + strings.Repeat("a", 64)
				if err := r.Status().Update(ctx, model); err != nil {
					t.Fatal(err)
				}
			case "Failed", "missing-digest":
				typ, target := batchv1.JobFailed, batchv1.JobFailureTarget
				now := metav1.Now()
				job.Status.StartTime = &now
				if phase == "missing-digest" {
					typ, target = batchv1.JobComplete, batchv1.JobSuccessCriteriaMet
					job.Status.CompletionTime = &now
					job.Status.Succeeded = 1
				}
				job.Status.Conditions = []batchv1.JobCondition{{Type: target, Status: corev1.ConditionTrue}, {Type: typ, Status: corev1.ConditionTrue}}
				if err := r.Status().Update(ctx, job); err != nil {
					t.Fatal(err)
				}
			case "invalid-spec":
				model.Spec.PrefetchOCI.Image = "invalid"
				if err := r.Update(ctx, model); err != nil {
					t.Fatal(err)
				}
			}
			reconcile()
			version := model.ResourceVersion
			time.Sleep(time.Second)
			reconcile()
			if model.ResourceVersion != version {
				t.Fatalf("steady %s publication rewrote status: ResourceVersion %s -> %s", phase, version, model.ResourceVersion)
			}
		})
	}
}

// A listing outage must not invoke the destructive invalid-spec failure path.
func TestOCIPrefetchListingFailurePreservesPublication(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		t.Run(fmt.Sprint(succeeded), func(t *testing.T) {
			r, model := ociPrefetchFixture(t)
			r.AllowedRemoteHosts = []string{"127.0.0.1"}
			broken := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if broken {
					http.Error(w, "unavailable", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, `[{"type":"file","path":"weights.gguf","size":1}]`)
			}))
			defer server.Close()
			ctx := context.Background()
			model.Spec.Source = "hf://org/model"
			model.Spec.Files = []string{"*.gguf"}
			model.Spec.SourceSecretRef = &corev1.LocalObjectReference{Name: "source"}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: model.Namespace}, Data: map[string][]byte{"HF_ENDPOINT": []byte(server.URL)}}
			if err := r.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
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
			if succeeded {
				model.Status.PrefetchOCI.Phase = inferencev1alpha1.PrefetchPhaseSucceeded
				model.Status.PrefetchOCI.Image = "oci://registry.example.com/models/test@sha256:" + strings.Repeat("a", 64)
				if err := r.Status().Update(ctx, model); err != nil {
					t.Fatal(err)
				}
				reconcile()
				model.Generation++
				model.Spec.PrefetchTolerations = []corev1.Toleration{{Key: "test", Operator: corev1.TolerationOpExists}}
				if err := r.Update(ctx, model); err != nil {
					t.Fatal(err)
				}
			}
			before := model.Status.PrefetchOCI.DeepCopy()
			broken = true
			reconcile()
			if !equality.Semantic.DeepEqual(before, model.Status.PrefetchOCI) {
				t.Fatalf("listing failure changed publication: before=%+v after=%+v", before, model.Status.PrefetchOCI)
			}
			job := &batchv1.Job{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: model.Namespace, Name: before.JobName}, job); err != nil {
				t.Fatalf("listing failure removed Job: %v", err)
			}
			if !job.DeletionTimestamp.IsZero() {
				t.Fatal("listing failure deleted Job")
			}
			if !meta.IsStatusConditionTrue(model.Status.Conditions, ConditionDegraded) {
				t.Fatal("listing failure not reported")
			}
			version := model.ResourceVersion
			reconcile()
			if version != model.ResourceVersion {
				t.Fatal("repeated listing failure rewrote status")
			}
			broken = false
			reconcile()
			if meta.IsStatusConditionTrue(model.Status.Conditions, ConditionDegraded) {
				t.Fatal("listing recovery left Model degraded")
			}
			if succeeded && model.Status.PrefetchOCI.Image != before.Image {
				t.Fatal("listing recovery lost published image")
			}
		})
	}
}

func TestOCIPrefetchInvalidFileManifest(t *testing.T) {
	r, model := ociPrefetchFixture(t)
	ctx := context.Background()
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: model.Namespace, Name: model.Status.PrefetchOCI.JobName}
	if err := r.Get(ctx, key, job); err != nil {
		t.Fatal(err)
	}
	job.Annotations[annotationOCIFiles] = "not-json"
	if err := r.Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(model.Status.Conditions, ConditionAvailable)
	if model.Status.Phase != PhaseFailed || condition == nil || condition.Reason != "InvalidOCIPrefetchResult" {
		t.Fatalf("invalid manifest not rejected: %+v", model.Status)
	}
	if err := r.Get(ctx, key, job); err == nil && job.DeletionTimestamp.IsZero() {
		t.Fatal("invalid Job not deleted")
	} else if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

func TestOCIPrefetchMixedURLs(t *testing.T) {
	requireInitShellEnvironment(t)
	r, model := ociPrefetchFixture(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/weights.gguf":
			_, _ = fmt.Fprint(w, "fixture weights")
		case "/bucket/encoder.bin":
			_, _ = fmt.Fprint(w, "fixture encoder")
		default:
			http.NotFound(w, req)
		}
	}))
	defer source.Close()
	registryServer := httptest.NewServer(registry.New())
	defer registryServer.Close()
	model.Spec.Source = ""
	model.Spec.Files = []string{source.URL + "/weights.gguf", "s3://bucket/encoder.bin"}
	model.Spec.PrefetchOCI.Image = strings.TrimPrefix(registryServer.URL, "http://") + "/models/mixed:v1"
	ctx := context.Background()
	if err := r.Update(ctx, model); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.reconcilePrefetch(ctx, model); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: model.Namespace, Name: model.Status.PrefetchOCI.JobName}, job); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	publisher := job.Spec.Template.Spec.Containers[0]
	var mappings []ocipublish.File
	for _, arg := range publisher.Args {
		if strings.HasPrefix(arg, "--files=") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "--files=")), &mappings); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(mappings) != 2 {
		t.Fatalf("publisher files=%+v", mappings)
	}
	downloadDir := filepath.Dir(filepath.Join(root, mappings[0].Path))
	if err := os.MkdirAll(downloadDir, 0700); err != nil {
		t.Fatal(err)
	}
	downloader := downloaderOf(t, job.Spec.Template.Spec.InitContainers).DeepCopy()
	// Run the container script on the host with its mount redirected to scratch space.
	downloader.Command[2] = strings.ReplaceAll(downloader.Command[2], "/models", root)
	if out, err := runDownloaderOnFS(t, downloader, downloadDir, "ext4", "AWS_ACCESS_KEY_ID=access", "AWS_SECRET_ACCESS_KEY=key", "AWS_REGION=us-east-1", "AWS_ENDPOINT_URL="+source.URL); err != nil {
		t.Fatalf("download: %v\n%s", err, out)
	}
	result := filepath.Join(root, "result")
	args := append(append([]string(nil), publisher.Args...), "--directory="+root, "--result="+result)
	if err := ocipublish.Run(ctx, args); err != nil {
		t.Fatal(err)
	}
	digest, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.NewDigest(strings.TrimPrefix(string(digest), "oci://"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := remote.Image(ref)
	if err != nil {
		t.Fatal(err)
	}
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, layer := range layers {
		stream, err := layer.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(stream)
		header, err := tr.Next()
		if err != nil {
			_ = stream.Close()
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = string(body)
	}
	want := map[string]string{"weights.gguf": "fixture weights", "encoder.bin": "fixture encoder"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published files=%v want=%v", got, want)
	}
}

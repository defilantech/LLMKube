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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// The InferenceService is created from a manifest, not the typed struct, so
// this file also compiles and runs on the base branch, where spec.schedulerName
// does not exist.

const schedulerNameFixtureImage = "ghcr.io/ggml-org/llama.cpp:server"

// schedulerNameModel creates the Ready Model the fixtures reference. The cache
// key is what makes the storage builder render a cache-backed pod.
func schedulerNameModel(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/models/scheduler-name.gguf",
			Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
				GPU:         &inferencev1alpha1.GPUSpec{Enabled: true, Count: 1, Vendor: "nvidia"},
			},
		},
	}
	if err := c.Create(ctx, model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	model.Status.Phase = PhaseReady
	model.Status.CacheKey = "scheduler-name-cache"
	model.Status.Path = "/models/scheduler-name-cache/scheduler-name.gguf"
	if err := c.Status().Update(ctx, model); err != nil {
		t.Fatalf("mark model ready: %v", err)
	}
}

// schedulerNameManifest is the manifest a user would apply. A nil schedulerName
// omits the key; a pointer to "" carries an explicit empty value, which the
// typed client's omitempty cannot express.
func schedulerNameManifest(name, modelName string, schedulerName *string) *unstructured.Unstructured {
	spec := map[string]any{
		"modelRef":  modelName,
		"runtime":   "llamacpp",
		"image":     schedulerNameFixtureImage,
		"resources": map[string]any{"gpu": int64(1)},
	}
	if schedulerName != nil {
		spec["schedulerName"] = *schedulerName
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "inference.llmkube.dev/v1alpha1",
		"kind":       "InferenceService",
		"metadata":   map[string]any{"name": name, "namespace": testNS},
		"spec":       spec,
	}}
}

func schedulerNameReconciler(c client.Client) *InferenceServiceReconciler {
	return &InferenceServiceReconciler{
		Client:             c,
		Scheme:             c.Scheme(),
		InitContainerImage: "docker.io/curlimages/curl:8.18.0",
		ModelCachePath:     "/models",
		ModelCacheMode:     ModelCacheModePerService,
	}
}

func reconcileSchedulerNameISvc(t *testing.T, r *InferenceServiceReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNS},
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// A user sets spec.schedulerName on a manifest; the apiserver must store it
// instead of pruning it, which is what a CRD without the field does.
func TestSchedulerNameStoredOnInferenceService(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	schedulerNameModel(t, c, "sched-stored-model")

	u := schedulerNameManifest("sched-stored", "sched-stored-model", ptrString("kai-scheduler"))
	if err := c.Create(ctx, u); err != nil {
		t.Fatalf("create isvc: %v", err)
	}

	stored := &unstructured.Unstructured{}
	stored.SetGroupVersionKind(u.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(u), stored); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	got, found, err := unstructured.NestedString(stored.Object, "spec", "schedulerName")
	if err != nil {
		t.Fatalf("read spec.schedulerName: %v", err)
	}
	if !found {
		t.Fatalf("spec.schedulerName was pruned on write: the CRD does not carry the field")
	}
	if got != "kai-scheduler" {
		t.Fatalf("spec.schedulerName = %q, want %q", got, "kai-scheduler")
	}
}

// The value is validated at admission the way a Pod's spec.schedulerName is:
// a DNS-1123 subdomain (empty is admitted, meaning the default scheduler).
func TestSchedulerNameRejectedAtAdmission(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	schedulerNameModel(t, c, "sched-admission-model")

	tooLong := strings.Repeat("a", 254)
	for _, tc := range []struct {
		name         string
		value        *string
		wantAdmitted bool
	}{
		{"kai-scheduler", ptrString("kai-scheduler"), true},
		{"dotted-subdomain", ptrString("kai.scheduler"), true},
		{"empty", ptrString(""), true},
		{"uppercase-and-underscore", ptrString("KAI_Scheduler"), false},
		{"underscore", ptrString("kai_scheduler"), false},
		{"too-long", ptrString(tooLong), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := schedulerNameManifest("sched-admission-"+tc.name, "sched-admission-model", tc.value)
			err := c.Create(ctx, u)
			if tc.wantAdmitted {
				if err != nil {
					t.Fatalf("schedulerName %q should be admitted: %v", *tc.value, err)
				}
				if delErr := c.Delete(ctx, u); delErr != nil {
					t.Fatalf("delete admitted isvc: %v", delErr)
				}
				return
			}
			if err == nil {
				t.Fatalf("schedulerName %q was admitted; a bad value must fail on the InferenceService instead of on the Deployment", *tc.value)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("schedulerName %q should be rejected as Invalid, got: %v", *tc.value, err)
			}
			if !strings.Contains(err.Error(), "spec.schedulerName") {
				t.Fatalf("the rejection does not name spec.schedulerName: %v", err)
			}
		})
	}
}

// The serving Deployment's pod template carries the field, so the named
// scheduler places the Pods.
func TestSchedulerNameReachesServingDeployment(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	schedulerNameModel(t, c, "sched-deploy-model")

	u := schedulerNameManifest("sched-deploy", "sched-deploy-model", ptrString("kai-scheduler"))
	if err := c.Create(ctx, u); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	reconcileSchedulerNameISvc(t, schedulerNameReconciler(c), "sched-deploy")

	var dep appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: "sched-deploy", Namespace: testNS}, &dep); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := dep.Spec.Template.Spec.SchedulerName; got != "kai-scheduler" {
		t.Fatalf("serving pod template schedulerName = %q, want %q", got, "kai-scheduler")
	}
}

// multiNode member Pods are specialised from the serving template, so they must
// carry the field too.
func TestSchedulerNameOnMultiNodeMemberPods(t *testing.T) {
	c, _, _ := sharedTestEnv(t, baseCRDPath)
	ctx := context.Background()
	schedulerNameModel(t, c, "sched-mn-model")
	psaPriorityClass(t, c)

	for _, name := range []string{"sched-mn-claim-0", "sched-mn-claim-1"} {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		if err := c.Create(ctx, pvc); err != nil {
			t.Fatalf("create claim %s: %v", name, err)
		}
	}

	u := schedulerNameManifest("sched-mn", "sched-mn-model", ptrString("kai-scheduler"))
	spec := u.Object["spec"].(map[string]any)
	spec["runtime"] = "vllm"
	spec["vllmConfig"] = map[string]any{"tensorParallelSize": int64(2)}
	spec["modelCache"] = map[string]any{"claimName": "sched-mn-claim-0"}
	spec["multiNode"] = map[string]any{
		"members": []any{
			map[string]any{"node": "node-a", "fabric": map[string]any{"address": "10.0.0.1"}},
			map[string]any{"node": "node-b", "fabric": map[string]any{"address": "10.0.0.2"},
				"modelCache": map[string]any{"claimName": "sched-mn-claim-1"}},
		},
	}
	if err := c.Create(ctx, u); err != nil {
		t.Fatalf("create multiNode isvc: %v", err)
	}
	reconcileSchedulerNameISvc(t, schedulerNameReconciler(c), "sched-mn")

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(testNS), client.MatchingLabels{LabelMultiNodeGroup: "sched-mn"}); err != nil {
		t.Fatalf("list member pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("got %d member pods, want 2", len(pods.Items))
	}
	for _, pod := range pods.Items {
		if got := pod.Spec.SchedulerName; got != "kai-scheduler" {
			t.Fatalf("member pod %s schedulerName = %q, want %q", pod.Name, got, "kai-scheduler")
		}
	}
}

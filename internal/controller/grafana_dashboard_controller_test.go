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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

const (
	testDashboardNS   = "llmkube-system"
	testCandidatesCM  = "llmkube-dashboard-candidates"
	testVLLMDashboard = "llmkube-vllm-dashboard"
)

// candidateManifest is what the chart writes into the source ConfigMap: a
// whole GrafanaDashboard carrying the runtime it needs.
func candidateManifest(name, runtimeName string) string {
	return fmt.Sprintf(`apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: %s
  namespace: %s
  labels:
    dashboards.llmkube.dev/managed-by: llmkube
  annotations:
    dashboards.llmkube.dev/requires-runtime: %q
spec:
  allowCrossNamespaceImport: true
  instanceSelector: {}
  configMapRef:
    name: llmkube-dashboards
    key: %s.json
`, name, testDashboardNS, runtimeName, name)
}

func dashboardScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add llmkube scheme: %v", err)
	}
	// grafana-operator's types are not vendored; the reconciler only ever
	// handles them as unstructured, so registering the GVK is enough.
	gvk := grafanaDashboardGVK()
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(s, gvk.GroupVersion())
	return s
}

// dashboardMapper makes the CRD look installed, which is what crdDetector
// reads. Tests for the absent case build a mapper without the kind.
func dashboardMapper(withDashboardKind bool) apimeta.RESTMapper {
	groups := []schema.GroupVersion{{Group: "inference.llmkube.dev", Version: "v1alpha1"}, {Group: "", Version: "v1"}}
	if withDashboardKind {
		groups = append(groups, grafanaDashboardGVK().GroupVersion())
	}
	mapper := apimeta.NewDefaultRESTMapper(groups)
	mapper.Add(inferencev1alpha1.GroupVersion.WithKind("InferenceService"), apimeta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, apimeta.RESTScopeNamespace)
	if withDashboardKind {
		mapper.Add(grafanaDashboardGVK(), apimeta.RESTScopeNamespace)
	}
	return mapper
}

func servingISvc(name, runtimeName string) *inferencev1alpha1.InferenceService {
	return isvcInPhase(name, runtimeName, inferencev1alpha1.PhaseReady)
}

func isvcInPhase(name, runtimeName, phase string) *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "a-model",
			Runtime:  runtimeName,
		},
		Status: inferencev1alpha1.InferenceServiceStatus{Phase: phase},
	}
}

func candidatesConfigMap(manifests map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testCandidatesCM, Namespace: testDashboardNS},
		Data:       manifests,
	}
}

func publishedDashboard(name string, labels map[string]string) *unstructured.Unstructured {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(grafanaDashboardGVK())
	live.SetName(name)
	live.SetNamespace(testDashboardNS)
	live.SetLabels(labels)
	return live
}

func newDashboardReconciler(t *testing.T, mapper apimeta.RESTMapper, objects ...client.Object) *GrafanaDashboardReconciler {
	t.Helper()

	s := dashboardScheme(t)
	return &GrafanaDashboardReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(s).
			WithRESTMapper(mapper).
			WithObjects(objects...).
			Build(),
		Scheme: s,
		Source: types.NamespacedName{Namespace: testDashboardNS, Name: testCandidatesCM},
	}
}

func getDashboard(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(grafanaDashboardGVK())
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testDashboardNS, Name: name}, live); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return live
}

func dashboardExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(grafanaDashboardGVK())
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testDashboardNS, Name: name}, live)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

// TestDashboardPublishedOnlyWhileItsRuntimeServes is the whole point of the
// reconciler: a dashboard reading vLLM metrics is blank until something serves
// vLLM, and blank is indistinguishable from an idle cluster (#1227).
func TestDashboardPublishedOnlyWhileItsRuntimeServes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		serving []client.Object
		want    bool
	}{
		{name: "no InferenceService at all", want: false},
		{name: "only another runtime", serving: []client.Object{servingISvc("llama", "llamacpp")}, want: false},
		{name: "the matching runtime", serving: []client.Object{servingISvc("v", "vllm")}, want: true},
		{
			name:    "matching runtime alongside others",
			serving: []client.Object{servingISvc("llama", "llamacpp"), servingISvc("v", "vllm")},
			want:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := append([]client.Object{
				candidatesConfigMap(map[string]string{
					"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
				}),
			}, tc.serving...)
			r := newDashboardReconciler(t, dashboardMapper(true), objects...)

			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := dashboardExists(t, r.Client, testVLLMDashboard); got != tc.want {
				t.Errorf("dashboard published = %v, want %v", got, tc.want)
			}
		})
	}
}

// The published set follows the cluster in both directions: the last vLLM
// service going away has to retire the dashboard it justified, or the blank
// panel comes back and never leaves.
func TestDashboardRetiredWhenItsRuntimeStopsServing(t *testing.T) {
	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		servingISvc("v", "vllm"),
	)
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !dashboardExists(t, r.Client, testVLLMDashboard) {
		t.Fatal("dashboard was not published while vLLM was serving")
	}

	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "default"}}
	if err := r.Delete(ctx, isvc); err != nil {
		t.Fatalf("delete InferenceService: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if dashboardExists(t, r.Client, testVLLMDashboard) {
		t.Error("dashboard survived the last vLLM InferenceService")
	}
}

// Retirement is scoped by the managed-by label the chart stamps on deferred
// manifests, so a dashboard someone applied by hand is never collateral.
func TestUnmanagedDashboardSurvivesRetirement(t *testing.T) {
	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		publishedDashboard(testVLLMDashboard, map[string]string{"app.kubernetes.io/managed-by": "Helm"}),
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !dashboardExists(t, r.Client, testVLLMDashboard) {
		t.Error("deleted a dashboard this operator did not publish")
	}
}

// The gap the retirement test alone leaves open: with a serving runtime,
// publish runs, and adopting a hand-authored dashboard would stamp our label
// onto it — which then makes it eligible for retirement later. Config would
// be overwritten and then deleted, in two steps and with no error anywhere.
func TestUnmanagedDashboardIsNotAdoptedByPublish(t *testing.T) {
	existing := publishedDashboard(testVLLMDashboard, map[string]string{"team": "platform"})
	existing.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "theirs"})
	existing.Object["spec"] = map[string]interface{}{"folder": "hand-authored"}

	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		servingISvc("v", "vllm"),
		existing,
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	live := getDashboard(t, r.Client, testVLLMDashboard)
	if got := live.GetLabels()[managedByLabel]; got != "" {
		t.Errorf("adopted a dashboard we did not publish: %s = %q", managedByLabel, got)
	}
	if folder, _, _ := unstructured.NestedString(live.Object, "spec", "folder"); folder != "hand-authored" {
		t.Errorf("overwrote a hand-authored spec: folder = %q", folder)
	}
	if got := live.GetAnnotations()["argocd.argoproj.io/tracking-id"]; got != "theirs" {
		t.Errorf("stripped a GitOps tracking annotation: %q", got)
	}
}

// Republishing must not strip metadata another controller owns: SetLabels and
// SetAnnotations replace the map wholesale, so a GitOps tracking annotation
// would be removed on every reconcile and re-added on every sync, forever.
func TestPublishPreservesForeignMetadata(t *testing.T) {
	existing := publishedDashboard(testVLLMDashboard, map[string]string{managedByLabel: managedByValue})
	existing.SetAnnotations(map[string]string{"argocd.argoproj.io/tracking-id": "ours"})

	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		servingISvc("v", "vllm"),
		existing,
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	live := getDashboard(t, r.Client, testVLLMDashboard)
	if got := live.GetAnnotations()["argocd.argoproj.io/tracking-id"]; got != "ours" {
		t.Errorf("tracking annotation = %q, want %q", got, "ours")
	}
	if got := live.GetAnnotations()[dashboardRuntimeAnnotation]; got != "vllm" {
		t.Errorf("%s = %q, want vllm", dashboardRuntimeAnnotation, got)
	}
}

// Without the CRD there is nothing to publish into, and without the flag the
// chart is applying dashboards itself. Both have to no-op rather than error,
// or a cluster with neither cannot run the operator.
func TestDashboardReconcilerNoOpsWhenUnconfigured(t *testing.T) {
	candidates := candidatesConfigMap(map[string]string{
		"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
	})

	t.Run("grafana-operator CRD absent", func(t *testing.T) {
		r := newDashboardReconciler(t, dashboardMapper(false), candidates, servingISvc("v", "vllm"))
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})

	t.Run("source flag unset", func(t *testing.T) {
		r := newDashboardReconciler(t, dashboardMapper(true), candidates, servingISvc("v", "vllm"))
		r.Source = types.NamespacedName{}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if dashboardExists(t, r.Client, testVLLMDashboard) {
			t.Error("published a dashboard with no source configured")
		}
	})

	t.Run("source ConfigMap missing", func(t *testing.T) {
		r := newDashboardReconciler(t, dashboardMapper(true), servingISvc("v", "vllm"))
		if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
}

// An unparsable key is a chart bug that requeueing cannot fix; the other
// dashboards still have to reconcile.
func TestUnparsableCandidateDoesNotBlockTheRest(t *testing.T) {
	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"broken.yaml":         "\tnot: [valid",
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		servingISvc("v", "vllm"),
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !dashboardExists(t, r.Client, testVLLMDashboard) {
		t.Error("a broken sibling key stopped the valid dashboard from publishing")
	}
}

// The runtime a dashboard waits for is carried onto the published object, so
// `kubectl get -o yaml` answers why it is here and not just that it is.
func TestPublishedDashboardKeepsItsRuntimeAnnotation(t *testing.T) {
	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
		}),
		servingISvc("v", "vllm"),
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(grafanaDashboardGVK())
	key := types.NamespacedName{Namespace: testDashboardNS, Name: testVLLMDashboard}
	if err := r.Get(context.Background(), key, live); err != nil {
		t.Fatalf("get published dashboard: %v", err)
	}
	if got := live.GetAnnotations()[dashboardRuntimeAnnotation]; got != "vllm" {
		t.Errorf("published %s = %q, want %q", dashboardRuntimeAnnotation, got, "vllm")
	}
}

// An InferenceService omitting spec.runtime serves llama.cpp, matching the
// runtime label its pods are scraped with.
func TestDefaultedRuntimeCountsAsServing(t *testing.T) {
	const llamaDashboard = "llmkube-llamacpp-dashboard"
	r := newDashboardReconciler(t, dashboardMapper(true),
		candidatesConfigMap(map[string]string{
			"llamacpp-dashboard.yaml": candidateManifest(llamaDashboard, "llamacpp"),
		}),
		servingISvc("unspecified", ""),
	)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !dashboardExists(t, r.Client, llamaDashboard) {
		t.Error("a service with no explicit runtime did not count as serving llamacpp")
	}
}

// A scaled-to-zero service still has an InferenceService object but no pods
// and so no series: the dashboard it would justify is exactly as blank as one
// for a runtime nobody ever deployed.
func TestTornDownServiceDoesNotCountAsServing(t *testing.T) {
	for _, tc := range []struct {
		phase string
		want  bool
	}{
		{phase: inferencev1alpha1.PhaseReady, want: true},
		{phase: inferencev1alpha1.PhaseCreating, want: true},
		{phase: inferencev1alpha1.PhaseWaitingForGPU, want: true},
		{phase: "", want: true},
		// Failed still intends to run pods and typically has crashlooping
		// ones, so it keeps its dashboard: retiring on Failed would flap it
		// out and back on every recovery, exactly when it is being watched.
		{phase: inferencev1alpha1.PhaseFailed, want: true},
		{phase: inferencev1alpha1.PhaseCached, want: true},
		{phase: inferencev1alpha1.PhaseDownloading, want: true},
		{phase: inferencev1alpha1.PhaseStopped, want: false},
		{phase: inferencev1alpha1.PhaseSuspended, want: false},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			r := newDashboardReconciler(t, dashboardMapper(true),
				candidatesConfigMap(map[string]string{
					"vllm-dashboard.yaml": candidateManifest(testVLLMDashboard, "vllm"),
				}),
				isvcInPhase("v", "vllm", tc.phase),
			)

			if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got := dashboardExists(t, r.Client, testVLLMDashboard); got != tc.want {
				t.Errorf("phase %q published = %v, want %v", tc.phase, got, tc.want)
			}
		})
	}
}

func TestParseDashboardSource(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		want    types.NamespacedName
		wantErr bool
	}{
		{flag: "", want: types.NamespacedName{}},
		{flag: "   ", want: types.NamespacedName{}},
		{flag: "llmkube-system/candidates", want: types.NamespacedName{Namespace: "llmkube-system", Name: "candidates"}},
		{flag: "candidates", wantErr: true},
		{flag: "/candidates", wantErr: true},
		{flag: "llmkube-system/", wantErr: true},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got, err := ParseDashboardSource(tc.flag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseDashboardSource(%q) error = %v, wantErr %v", tc.flag, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ParseDashboardSource(%q) = %v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}

package controller

import (
	"context"
	"path/filepath"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Plain Go tests (not the ginkgo suite) for the same reason as the gateway
// tests: precise control over whether the Pyrra CRD is registered.

const pyrraTestCRDDir = "../../test/crd/pyrra"

func startSLOTestEnv(t *testing.T, withPyrraCRD bool) (client.Client, *rest.Config, func()) {
	t.Helper()
	crdPaths := []string{filepath.Join("..", "..", "config", "crd", "bases")}
	if withPyrraCRD {
		crdPaths = append(crdPaths, pyrraTestCRDDir)
	}
	env := &envtest.Environment{CRDDirectoryPaths: crdPaths, ErrorIfCRDPathMissing: true}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	s := scheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c, cfg, func() { _ = env.Stop() }
}

func newSLOReconciler(t *testing.T, cfg *rest.Config, enabled bool) *InferenceServiceSLOReconciler {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconciler client: %v", err)
	}
	return &InferenceServiceSLOReconciler{Client: c, Scheme: scheme.Scheme, Enabled: enabled}
}

// nolint:unparam // runtime is parameterized for clarity at call sites even though existing tests all use "llamacpp"
func createSLOISvc(t *testing.T, c client.Client, name, runtime string, slo *inferencev1alpha1.SLOSpec) *inferencev1alpha1.InferenceService {
	t.Helper()
	isvc := &inferencev1alpha1.InferenceService{}
	isvc.Name = name
	isvc.Namespace = "default"
	isvc.Spec.ModelRef = name + "-model"
	isvc.Spec.Runtime = runtime
	isvc.Spec.SLO = slo
	if err := c.Create(context.Background(), isvc); err != nil {
		t.Fatalf("create isvc: %v", err)
	}
	return isvc
}

func reconcileSLO(t *testing.T, r *InferenceServiceSLOReconciler, isvc *inferencev1alpha1.InferenceService) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(isvc)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// nolint:unparam // ns is parameterized for clarity at call sites even though existing tests all use "default"
func getSLO(t *testing.T, c client.Client, ns, name string) (*unstructured.Unstructured, error) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(pyrraSLOGVK())
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u)
	return u, err
}

func TestSLOReconcile_AvailabilityCreatesResource(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()
	isvc := createSLOISvc(t, c, "avail-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99.5"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)

	u, err := getSLO(t, c, "default", "avail-isvc-availability")
	if err != nil {
		t.Fatalf("SLO resource not created: %v", err)
	}
	if len(u.GetOwnerReferences()) != 1 || u.GetOwnerReferences()[0].Name != "avail-isvc" {
		t.Errorf("missing/wrong owner reference: %+v", u.GetOwnerReferences())
	}
	spec := u.Object["spec"].(map[string]interface{})
	if spec["target"] != "99.5" {
		t.Errorf("target: %v", spec["target"])
	}

	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, SLOConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "SLOCreated" {
		t.Errorf("expected SLOReady=True/SLOCreated, got %+v", cond)
	}
}

func TestSLOReconcile_UnsupportedLatencyRuntime(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()
	isvc := createSLOISvc(t, c, "lat-llama", "llamacpp",
		&inferencev1alpha1.SLOSpec{Objective: "95", Indicator: "latency", LatencyThreshold: "2"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)

	if _, err := getSLO(t, c, "default", "lat-llama-latency"); err == nil {
		t.Fatalf("SLO resource should not exist for unsupported runtime")
	}
	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, SLOConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "IndicatorUnsupportedForRuntime" {
		t.Errorf("expected SLOReady=False/IndicatorUnsupportedForRuntime, got %+v", cond)
	}
}

func TestSLOReconcile_UnsetDeletesResource(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()
	isvc := createSLOISvc(t, c, "unset-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)
	if _, err := getSLO(t, c, "default", "unset-isvc-availability"); err != nil {
		t.Fatalf("precondition: SLO should exist: %v", err)
	}

	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	fresh.Spec.SLO = nil
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("unset slo: %v", err)
	}
	reconcileSLO(t, r, fresh)

	if _, err := getSLO(t, c, "default", "unset-isvc-availability"); err == nil {
		t.Fatalf("SLO resource should be deleted after spec.slo removal")
	}
}

func TestSLOReconcile_RenameDeletesStale(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()
	isvc := createSLOISvc(t, c, "rename-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)

	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	fresh.Spec.SLO.Name = "renamed-slo"
	if err := c.Update(context.Background(), fresh); err != nil {
		t.Fatalf("rename: %v", err)
	}
	reconcileSLO(t, r, fresh)

	if _, err := getSLO(t, c, "default", "renamed-slo"); err != nil {
		t.Fatalf("renamed SLO missing: %v", err)
	}
	if _, err := getSLO(t, c, "default", "rename-isvc-availability"); err == nil {
		t.Fatalf("stale SLO should be deleted after rename")
	}
}

func TestSLOReconcile_CRDsAbsentIsCleanNoOp(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, false)
	defer stop()
	isvc := createSLOISvc(t, c, "absent-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)

	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, SLOConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "PyrraNotInstalled" {
		t.Errorf("expected SLOReady=False/PyrraNotInstalled, got %+v", cond)
	}
}

func TestSLOReconcile_DisabledSetsCondition(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()
	isvc := createSLOISvc(t, c, "disabled-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99"})
	r := newSLOReconciler(t, cfg, false)
	reconcileSLO(t, r, isvc)

	if _, err := getSLO(t, c, "default", "disabled-isvc-availability"); err == nil {
		t.Fatalf("SLO resource should not be created when integration disabled")
	}
	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, SLOConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "IntegrationDisabled" {
		t.Errorf("expected SLOReady=False/IntegrationDisabled, got %+v", cond)
	}
}

func TestSLOReconcile_MetalSetsDataSourceCondition(t *testing.T) {
	c, cfg, stop := startSLOTestEnv(t, true)
	defer stop()

	model := &inferencev1alpha1.Model{}
	model.Name = "metal-isvc-model"
	model.Namespace = "default"
	model.Spec.Source = "https://example.com/m.gguf"
	model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{Accelerator: "metal"}
	if err := c.Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}

	isvc := createSLOISvc(t, c, "metal-isvc", "llamacpp", &inferencev1alpha1.SLOSpec{Objective: "99"})
	r := newSLOReconciler(t, cfg, true)
	reconcileSLO(t, r, isvc)

	// Rendered anyway (render + warn per the spec).
	if _, err := getSLO(t, c, "default", "metal-isvc-availability"); err != nil {
		t.Fatalf("SLO should render for Metal ISvc: %v", err)
	}
	fresh := &inferencev1alpha1.InferenceService{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(isvc), fresh)
	cond := apimeta.FindStatusCondition(fresh.Status.Conditions, SLOConditionDataSource)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "OffClusterRuntime" {
		t.Errorf("expected SLODataSourceAvailable=False/OffClusterRuntime, got %+v", cond)
	}
}

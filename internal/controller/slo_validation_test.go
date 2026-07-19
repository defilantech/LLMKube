package controller

import (
	"context"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// CEL/default behavior for spec.slo is enforced by the API server, so these
// are envtest tests, not unit tests. Plain Go (not the ginkgo suite) to match
// the gateway tests' per-case environment style.

func startValidationTestEnv(t *testing.T) (client.Client, func()) {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := getFirstFoundEnvTestBinaryDir(); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	s := scheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c, func() { _ = env.Stop() }
}

func sloISvc(name string, slo *inferencev1alpha1.SLOSpec) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{}
	isvc.Name = name
	isvc.Namespace = "default"
	isvc.Spec.ModelRef = "test-model"
	isvc.Spec.SLO = slo
	return isvc
}

func TestSLOValidation(t *testing.T) {
	c, stop := startValidationTestEnv(t)
	defer stop()
	ctx := context.Background()

	cases := []struct {
		name    string
		slo     *inferencev1alpha1.SLOSpec
		wantErr bool
	}{
		{"minimal valid", &inferencev1alpha1.SLOSpec{Objective: "99.5"}, false},
		{"objective too low", &inferencev1alpha1.SLOSpec{Objective: "49.9"}, true},
		{"objective too high", &inferencev1alpha1.SLOSpec{Objective: "100"}, true},
		{"objective not a number", &inferencev1alpha1.SLOSpec{Objective: "banana"}, true},
		{"latency without threshold", &inferencev1alpha1.SLOSpec{Objective: "99", Indicator: "latency"}, true},
		{"latency with threshold", &inferencev1alpha1.SLOSpec{Objective: "99", Indicator: "latency", LatencyThreshold: "2"}, false},
		{"bad window unit", &inferencev1alpha1.SLOSpec{Objective: "99", Window: "28x"}, true},
		{"bad indicator", &inferencev1alpha1.SLOSpec{Objective: "99", Indicator: "latency_p95"}, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Create(ctx, sloISvc(fmtName("slo-val", i), tc.slo))
			if tc.wantErr && err == nil {
				t.Fatalf("expected admission rejection, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected accept, got: %v", err)
			}
		})
	}

	// Defaulting: window and indicator are stamped by the API server.
	got := &inferencev1alpha1.InferenceService{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "slo-val-0"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.SLO.Window != "28d" || got.Spec.SLO.Indicator != "availability" {
		t.Errorf("defaults not applied: window=%q indicator=%q", got.Spec.SLO.Window, got.Spec.SLO.Indicator)
	}
}

func fmtName(prefix string, i int) string {
	return prefix + "-" + string(rune('0'+i)) //nolint:gosec // i is a small test-case index (0-9), never near int32 overflow
}

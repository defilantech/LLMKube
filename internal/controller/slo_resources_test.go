package controller

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func sloTestISvc(slo *inferencev1alpha1.SLOSpec, runtime string) *inferencev1alpha1.InferenceService {
	isvc := &inferencev1alpha1.InferenceService{}
	isvc.Name = "tinyllama"
	isvc.Namespace = "prod"
	isvc.Spec.ModelRef = "tinyllama-model"
	isvc.Spec.Runtime = runtime
	isvc.Spec.SLO = slo
	return isvc
}

func TestSLOResourceName(t *testing.T) {
	explicit := sloTestISvc(&inferencev1alpha1.SLOSpec{Name: "my-slo", Objective: "99.5", Indicator: "availability"}, "llamacpp")
	if got := sloResourceName(explicit); got != "my-slo" {
		t.Errorf("explicit name: got %q", got)
	}
	defaulted := sloTestISvc(&inferencev1alpha1.SLOSpec{Objective: "99.5", Indicator: "availability"}, "llamacpp")
	if got := sloResourceName(defaulted); got != "tinyllama-availability" {
		t.Errorf("defaulted name: got %q", got)
	}
}

func TestSLOIndicatorSupported(t *testing.T) {
	cases := []struct {
		runtime, indicator string
		want               bool
	}{
		{"llamacpp", "availability", true},
		{"vllm", "availability", true},
		{"sglang", "availability", true},
		{"vllm", "latency", true},
		{"llamacpp", "latency", false},
		{"tgi", "latency", false},
		{"generic", "latency", false},
	}
	for _, tc := range cases {
		if got := sloIndicatorSupported(tc.runtime, tc.indicator); got != tc.want {
			t.Errorf("supported(%s,%s)=%v want %v", tc.runtime, tc.indicator, got, tc.want)
		}
	}
}

func TestNewServiceLevelObjective_Availability(t *testing.T) {
	isvc := sloTestISvc(&inferencev1alpha1.SLOSpec{Objective: "99.5", Window: "28d", Indicator: "availability"}, "llamacpp")
	u := newServiceLevelObjective(isvc)

	if u.GetAPIVersion() != "pyrra.dev/v1alpha1" || u.GetKind() != "ServiceLevelObjective" {
		t.Fatalf("wrong GVK: %s/%s", u.GetAPIVersion(), u.GetKind())
	}
	if u.GetName() != "tinyllama-availability" || u.GetNamespace() != "prod" {
		t.Fatalf("wrong name/ns: %s/%s", u.GetNamespace(), u.GetName())
	}
	if u.GetLabels()[sloISvcLabel] != "tinyllama" || u.GetLabels()["app.kubernetes.io/managed-by"] != "llmkube" {
		t.Fatalf("wrong labels: %v", u.GetLabels())
	}
	spec := u.Object["spec"].(map[string]interface{})
	if spec["target"] != "99.5" || spec["window"] != "28d" {
		t.Fatalf("wrong target/window: %v", spec)
	}
	metric := spec["indicator"].(map[string]interface{})["bool_gauge"].(map[string]interface{})["metric"]
	want := `up{namespace="prod",service="tinyllama"}`
	if metric != want {
		t.Fatalf("bool_gauge metric:\n got  %v\n want %v", metric, want)
	}
}

func TestNewServiceLevelObjective_LatencyVLLM(t *testing.T) {
	isvc := sloTestISvc(&inferencev1alpha1.SLOSpec{Objective: "95", Window: "7d", Indicator: "latency", LatencyThreshold: "2"}, "vllm")
	u := newServiceLevelObjective(isvc)

	spec := u.Object["spec"].(map[string]interface{})
	lat := spec["indicator"].(map[string]interface{})["latency"].(map[string]interface{})
	success := lat["success"].(map[string]interface{})["metric"]
	total := lat["total"].(map[string]interface{})["metric"]
	wantSuccess := `vllm:e2e_request_latency_seconds_bucket{namespace="prod",service="tinyllama",le="2"}`
	wantTotal := `vllm:e2e_request_latency_seconds_count{namespace="prod",service="tinyllama"}`
	if success != wantSuccess {
		t.Fatalf("success:\n got  %v\n want %v", success, wantSuccess)
	}
	if total != wantTotal {
		t.Fatalf("total:\n got  %v\n want %v", total, wantTotal)
	}
}

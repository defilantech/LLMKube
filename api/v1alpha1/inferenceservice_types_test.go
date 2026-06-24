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

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fullyPopulatedInferenceService returns an InferenceService that exercises
// every non-trivial spec and status path. Used as the canonical test fixture
// so round-trip and deep-copy coverage stays in sync as the type evolves.
func fullyPopulatedInferenceService() *InferenceService {
	return &InferenceService{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inference.llmkube.dev/v1alpha1",
			Kind:       "InferenceService",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen3-coder-isvc",
			Namespace: "platform",
		},
		Spec: InferenceServiceSpec{
			ModelRef: "qwen3-coder",
			Runtime:  "llamacpp",
			Replicas: ptrInt32(2),
			Image:    "ghcr.io/ggml-org/llama.cpp:server",
			Endpoint: &EndpointSpec{
				Port: 8080,
				Path: "/v1/chat/completions",
				Type: "ClusterIP",
			},
			Resources: &InferenceResourceRequirements{
				GPU:        1,
				CPU:        "4",
				Memory:     "32Gi",
				HostMemory: "64Gi",
				GPUMemory:  "16Gi",
			},
			ContextSize:        ptrInt32(8192),
			ParallelSlots:      ptrInt32(4),
			FlashAttention:     ptrBool(true),
			Jinja:              ptrBool(true),
			CacheTypeK:         "f16",
			CacheTypeV:         "f16",
			MoeCPUOffload:      ptrBool(false),
			NoKvOffload:        ptrBool(false),
			NoWarmup:           ptrBool(false),
			ReasoningBudget:    ptrInt32(0),
			BatchSize:          ptrInt32(512),
			UBatchSize:         ptrInt32(128),
			ExtraArgs:          []string{"--seed", "42"},
			Priority:           "high",
			EvictionProtection: ptrBool(true),
			PodAnnotations:     map[string]string{"cost-center": "ml"},
			PodLabels:          map[string]string{"team": "platform"},
			Tolerations: []corev1.Toleration{
				{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
			},
			NodeSelector: map[string]string{"gpu": "true"},
			RopeScaling: &RopeScalingSpec{
				Type:            "yarn",
				Factor:          "2.0",
				OriginalContext: ptrInt32(131072),
			},
			VLLMConfig: &VLLMConfig{
				TensorParallelSize: ptrInt32(2),
				MaxModelLen:        ptrInt32(131072),
				Quantization:       "awq",
				Dtype:              "bfloat16",
				AttentionBackend:   "FLASHINFER",
			},
			Autoscaling: &AutoscalingSpec{
				MinReplicas: ptrInt32(1),
				MaxReplicas: 5,
				Metrics: []MetricSpec{
					{
						Type:               "Pods",
						Name:               "llamacpp:requests_processing",
						TargetAverageValue: ptrString("2"),
					},
				},
			},
			Disruption: &DisruptionSpec{
				ProtectStartup: ptrBool(true),
				ProtectAlways:  ptrBool(false),
			},
		},
		Status: InferenceServiceStatus{
			Phase:           "Ready",
			ReadyReplicas:   2,
			DesiredReplicas: 2,
			Replicas:        2,
			Endpoint:        "http://qwen3-coder-isvc.platform.svc.cluster.local:8080/v1/chat/completions",
			ModelReady:      true,
			Conditions: []metav1.Condition{
				{Type: "Available", Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}
}

// TestInferenceServiceDeepCopyIndependence verifies the generated DeepCopy
// methods produce a fully independent clone.
func TestInferenceServiceDeepCopyIndependence(t *testing.T) {
	orig := fullyPopulatedInferenceService()
	clone := orig.DeepCopy()

	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("clone differs from original immediately after DeepCopy")
	}

	// Mutate slices on the clone.
	clone.Status.Conditions[0].Reason = "MUTATED"
	clone.Status.Conditions = append(clone.Status.Conditions, metav1.Condition{Type: "Extra"})

	if got := orig.Status.Conditions[0].Reason; got != "Ready" {
		t.Errorf("original Status.Conditions[0].Reason = %q; want %q", got, "Ready")
	}
	if len(orig.Status.Conditions) != 1 {
		t.Errorf("len(original Status.Conditions) = %d; want 1", len(orig.Status.Conditions))
	}

	// Mutate maps on the clone.
	clone.Spec.PodAnnotations["cost-center"] = "MUTATED"
	clone.Spec.PodLabels["team"] = "MUTATED"

	if got := orig.Spec.PodAnnotations["cost-center"]; got != "ml" {
		t.Errorf("original Spec.PodAnnotations[cost-center] = %q; want %q", got, "ml")
	}
	if got := orig.Spec.PodLabels["team"]; got != "platform" {
		t.Errorf("original Spec.PodLabels[team] = %q; want %q", got, "platform")
	}

	// Mutate pointer fields on the clone.
	*clone.Spec.Replicas = 0
	clone.Spec.VLLMConfig.TensorParallelSize = ptrInt32(4)

	if got := *orig.Spec.Replicas; got != 2 {
		t.Errorf("original Spec.Replicas = %d; want 2", got)
	}
	if got := *orig.Spec.VLLMConfig.TensorParallelSize; got != 2 {
		t.Errorf("original Spec.VLLMConfig.TensorParallelSize = %d; want 2", got)
	}
}

// TestInferenceServiceJSONRoundTrip confirms every field marshals and
// unmarshals through JSON without loss.
func TestInferenceServiceJSONRoundTrip(t *testing.T) {
	orig := fullyPopulatedInferenceService()

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got InferenceService
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(orig, &got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", orig, &got)
	}
}

// TestInferenceServiceListDeepCopy verifies the list type's DeepCopy is
// independent.
func TestInferenceServiceListDeepCopy(t *testing.T) {
	list := &InferenceServiceList{
		Items: []InferenceService{
			*fullyPopulatedInferenceService(),
		},
	}
	clone := list.DeepCopy()

	if !reflect.DeepEqual(list, clone) {
		t.Fatal("InferenceServiceList clone differs from original")
	}

	clone.Items[0].Spec.ModelRef = "MUTATED"
	if got := list.Items[0].Spec.ModelRef; got != "qwen3-coder" {
		t.Errorf("original Items[0].Spec.ModelRef = %q; want %q", got, "qwen3-coder")
	}
}

// TestInferenceServiceSchemeRegistration confirms the InferenceService and
// InferenceServiceList types are registered with the package's SchemeBuilder.
func TestInferenceServiceSchemeRegistration(t *testing.T) {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("SchemeBuilder.Build: %v", err)
	}
	gvks, _, err := scheme.ObjectKinds(&InferenceService{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(InferenceService): %v", err)
	}
	if len(gvks) == 0 {
		t.Fatal("InferenceService not registered in scheme")
	}
	if gvks[0].Group != "inference.llmkube.dev" || gvks[0].Version != "v1alpha1" || gvks[0].Kind != "InferenceService" {
		t.Errorf("unexpected GVK %s", gvks[0])
	}
}

// TestInferenceServiceMinimalSpec verifies a minimal InferenceService
// (only required fields) marshals and unmarshals correctly.
func TestInferenceServiceMinimalSpec(t *testing.T) {
	isvc := &InferenceService{
		Spec: InferenceServiceSpec{
			ModelRef: "my-model",
		},
	}
	data, err := json.Marshal(isvc.Spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got InferenceServiceSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ModelRef != "my-model" {
		t.Errorf("ModelRef = %q; want %q", got.ModelRef, "my-model")
	}
}

// TestInferenceServiceRuntimeEnum verifies the documented runtime values
// are accepted by the type.
func TestInferenceServiceRuntimeEnum(t *testing.T) {
	runtimes := []string{"llamacpp", "personaplex", "vllm", "tgi", "generic"}
	for _, rt := range runtimes {
		t.Run(rt, func(t *testing.T) {
			isvc := &InferenceService{
				Spec: InferenceServiceSpec{
					ModelRef: "m",
					Runtime:  rt,
				},
			}
			data, err := json.Marshal(isvc.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got InferenceServiceSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Runtime != rt {
				t.Errorf("Runtime = %q; want %q", got.Runtime, rt)
			}
		})
	}
}

// TestInferenceServicePriorityEnum verifies the documented priority values
// are accepted by the type.
func TestInferenceServicePriorityEnum(t *testing.T) {
	priorities := []string{"critical", "high", "normal", "low", "batch"}
	for _, p := range priorities {
		t.Run(p, func(t *testing.T) {
			isvc := &InferenceService{
				Spec: InferenceServiceSpec{
					ModelRef: "m",
					Priority: p,
				},
			}
			data, err := json.Marshal(isvc.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got InferenceServiceSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Priority != p {
				t.Errorf("Priority = %q; want %q", got.Priority, p)
			}
		})
	}
}

// TestInferenceServiceStatusPhaseEnum verifies the documented phase values
// are accepted by the type.
func TestInferenceServiceStatusPhaseEnum(t *testing.T) {
	phases := []string{"Pending", "Creating", "Progressing", "Ready", "WaitingForGPU", "Stopped", "Failed"}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			isvc := &InferenceService{
				Status: InferenceServiceStatus{Phase: phase},
			}
			data, err := json.Marshal(isvc.Status)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got InferenceServiceStatus
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Phase != phase {
				t.Errorf("Phase = %q; want %q", got.Phase, phase)
			}
		})
	}
}

// TestEndpointSpecRoundTrip verifies EndpointSpec round-trips through JSON.
func TestEndpointSpecRoundTrip(t *testing.T) {
	ep := EndpointSpec{
		Port: 8080,
		Path: "/v1/chat/completions",
		Type: "ClusterIP",
	}
	data, err := json.Marshal(ep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got EndpointSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(ep, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", ep, got)
	}
}

// TestEndpointTypeEnum verifies the documented endpoint type values are
// accepted by the type.
func TestEndpointTypeEnum(t *testing.T) {
	types := []string{"ClusterIP", "NodePort", "LoadBalancer"}
	for _, tp := range types {
		t.Run(tp, func(t *testing.T) {
			ep := EndpointSpec{Type: tp}
			data, err := json.Marshal(ep)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got EndpointSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Type != tp {
				t.Errorf("Type = %q; want %q", got.Type, tp)
			}
		})
	}
}

// TestRopeScalingSpecRoundTrip verifies RopeScalingSpec round-trips through
// JSON.
func TestRopeScalingSpecRoundTrip(t *testing.T) {
	rs := RopeScalingSpec{
		Type:            "yarn",
		Factor:          "2.0",
		OriginalContext: ptrInt32(131072),
	}
	data, err := json.Marshal(rs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got RopeScalingSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(rs, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", rs, got)
	}
}

// TestRopeScalingTypeEnum verifies the documented rope scaling type values
// are accepted by the type.
func TestRopeScalingTypeEnum(t *testing.T) {
	types := []string{"linear", "yarn", "longrope"}
	for _, tp := range types {
		t.Run(tp, func(t *testing.T) {
			rs := RopeScalingSpec{Type: RopeScalingType(tp)}
			data, err := json.Marshal(rs)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got RopeScalingSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if string(got.Type) != tp {
				t.Errorf("Type = %q; want %q", got.Type, tp)
			}
		})
	}
}

// TestVLLMConfigRoundTrip verifies VLLMConfig round-trips through JSON.
func TestVLLMConfigRoundTrip(t *testing.T) {
	cfg := VLLMConfig{
		TensorParallelSize: ptrInt32(2),
		MaxModelLen:        ptrInt32(131072),
		Quantization:       "awq",
		Dtype:              "bfloat16",
		AttentionBackend:   "FLASHINFER",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got VLLMConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", cfg, got)
	}
}

// TestVLLMQuantizationEnum verifies the documented vLLM quantization values
// are accepted by the type.
func TestVLLMQuantizationEnum(t *testing.T) {
	qs := []string{"awq", "gptq", "squeezellm", "fp8", "nvfp4", "compressed-tensors"}
	for _, q := range qs {
		t.Run(q, func(t *testing.T) {
			cfg := VLLMConfig{Quantization: q}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got VLLMConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Quantization != q {
				t.Errorf("Quantization = %q; want %q", got.Quantization, q)
			}
		})
	}
}

// TestVLLMDtypeEnum verifies the documented vLLM dtype values are accepted
// by the type.
func TestVLLMDtypeEnum(t *testing.T) {
	dtypes := []string{"auto", "float16", "bfloat16"}
	for _, dt := range dtypes {
		t.Run(dt, func(t *testing.T) {
			cfg := VLLMConfig{Dtype: dt}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got VLLMConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Dtype != dt {
				t.Errorf("Dtype = %q; want %q", got.Dtype, dt)
			}
		})
	}
}

// TestVLLMAttentionBackendEnum verifies the documented attention backend
// values are accepted by the type.
func TestVLLMAttentionBackendEnum(t *testing.T) {
	backends := []string{"FLASH_ATTN", "FLASHINFER", "XFORMERS", "flashinfer", "flash_attn", "xformers", "torch_sdpa"}
	for _, b := range backends {
		t.Run(b, func(t *testing.T) {
			cfg := VLLMConfig{AttentionBackend: b}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got VLLMConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.AttentionBackend != b {
				t.Errorf("AttentionBackend = %q; want %q", got.AttentionBackend, b)
			}
		})
	}
}

// TestTGIConfigRoundTrip verifies TGIConfig round-trips through JSON.
func TestTGIConfigRoundTrip(t *testing.T) {
	cfg := TGIConfig{
		Quantize:       "bitsandbytes",
		MaxInputLength: ptrInt32(4096),
		MaxTotalTokens: ptrInt32(8192),
		Dtype:          "float16",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got TGIConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", cfg, got)
	}
}

// TestTGIQuantizeEnum verifies the documented TGI quantize values are
// accepted by the type.
func TestTGIQuantizeEnum(t *testing.T) {
	qs := []string{"bitsandbytes", "gptq", "awq", "eetq"}
	for _, q := range qs {
		t.Run(q, func(t *testing.T) {
			cfg := TGIConfig{Quantize: q}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got TGIConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Quantize != q {
				t.Errorf("Quantize = %q; want %q", got.Quantize, q)
			}
		})
	}
}

// TestTGIDtypeEnum verifies the documented TGI dtype values are accepted
// by the type.
func TestTGIDtypeEnum(t *testing.T) {
	dtypes := []string{"float16", "bfloat16"}
	for _, dt := range dtypes {
		t.Run(dt, func(t *testing.T) {
			cfg := TGIConfig{Dtype: dt}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got TGIConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Dtype != dt {
				t.Errorf("Dtype = %q; want %q", got.Dtype, dt)
			}
		})
	}
}

// TestAutoscalingSpecRoundTrip verifies AutoscalingSpec round-trips through
// JSON.
func TestAutoscalingSpecRoundTrip(t *testing.T) {
	as := AutoscalingSpec{
		MinReplicas: ptrInt32(1),
		MaxReplicas: 5,
		Metrics: []MetricSpec{
			{
				Type:               "Pods",
				Name:               "llamacpp:requests_processing",
				TargetAverageValue: ptrString("2"),
			},
		},
	}
	data, err := json.Marshal(as)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got AutoscalingSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(as, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", as, got)
	}
}

// TestMetricSpecTypeEnum verifies the documented metric type values are
// accepted by the type.
func TestMetricSpecTypeEnum(t *testing.T) {
	types := []string{"Pods", "Resource"}
	for _, tp := range types {
		t.Run(tp, func(t *testing.T) {
			ms := MetricSpec{Type: tp, Name: "m"}
			data, err := json.Marshal(ms)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got MetricSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Type != tp {
				t.Errorf("Type = %q; want %q", got.Type, tp)
			}
		})
	}
}

// TestDisruptionSpecRoundTrip verifies DisruptionSpec round-trips through
// JSON.
func TestDisruptionSpecRoundTrip(t *testing.T) {
	ds := DisruptionSpec{
		ProtectStartup: ptrBool(true),
		ProtectAlways:  ptrBool(false),
	}
	data, err := json.Marshal(ds)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got DisruptionSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(ds, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", ds, got)
	}
}

// TestInferenceResourceRequirementsRoundTrip verifies
// InferenceResourceRequirements round-trips through JSON.
func TestInferenceResourceRequirementsRoundTrip(t *testing.T) {
	res := InferenceResourceRequirements{
		GPU:        1,
		CPU:        "4",
		Memory:     "32Gi",
		HostMemory: "64Gi",
		GPUMemory:  "16Gi",
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got InferenceResourceRequirements
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(res, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", res, got)
	}
}

// TestGatewaySpecRoundTrip verifies GatewaySpec round-trips through JSON.
func TestGatewaySpecRoundTrip(t *testing.T) {
	gs := GatewaySpec{
		Enabled: true,
		GatewayRef: GatewayReference{
			Name:      "my-gateway",
			Namespace: "gateway-ns",
		},
		ModelName: "my-model",
	}
	data, err := json.Marshal(gs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GatewaySpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(gs, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", gs, got)
	}
}

// TestGatewayStatusRoundTrip verifies GatewayStatus round-trips through JSON.
func TestGatewayStatusRoundTrip(t *testing.T) {
	gs := GatewayStatus{
		RouteReady:  true,
		ModelName:   "my-model",
		Endpoint:    "http://gateway.example.com",
		AuthEnabled: true,
	}
	data, err := json.Marshal(gs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GatewayStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(gs, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", gs, got)
	}
}

// TestSpeculativeConfigRoundTrip verifies SpeculativeConfig round-trips
// through JSON.
func TestSpeculativeConfigRoundTrip(t *testing.T) {
	sc := SpeculativeConfig{
		Enabled:              ptrBool(true),
		Model:                "draft-model",
		NumSpeculativeTokens: ptrInt32(4),
	}
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got SpeculativeConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(sc, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", sc, got)
	}
}

// TestPersonaPlexConfigRoundTrip verifies PersonaPlexConfig round-trips
// through JSON.
func TestPersonaPlexConfigRoundTrip(t *testing.T) {
	cfg := PersonaPlexConfig{
		Quantize4Bit: ptrBool(true),
		CPUOffload:   ptrBool(false),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got PersonaPlexConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", cfg, got)
	}
}

// TestCacheTypeEnum verifies the documented cache type values are accepted
// by the type.
func TestCacheTypeEnum(t *testing.T) {
	types := []string{"f16", "f32", "q8_0", "q4_0", "q4_1", "q5_0", "q5_1", "iq4_nl"}
	for _, tp := range types {
		t.Run(tp, func(t *testing.T) {
			isvc := &InferenceService{
				Spec: InferenceServiceSpec{
					ModelRef:   "m",
					CacheTypeK: tp,
				},
			}
			data, err := json.Marshal(isvc.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got InferenceServiceSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.CacheTypeK != tp {
				t.Errorf("CacheTypeK = %q; want %q", got.CacheTypeK, tp)
			}
		})
	}
}

// TestVLLMKVCacheDtypeEnum verifies the documented vLLM KV cache dtype
// values are accepted by the type.
func TestVLLMKVCacheDtypeEnum(t *testing.T) {
	dtypes := []string{"auto", "fp8_e5m2", "fp8_e4m3"}
	for _, dt := range dtypes {
		t.Run(dt, func(t *testing.T) {
			cfg := VLLMConfig{KVCacheDtype: ptrString(dt)}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got VLLMConfig
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.KVCacheDtype == nil || *got.KVCacheDtype != dt {
				t.Errorf("KVCacheDtype = %v; want %q", got.KVCacheDtype, dt)
			}
		})
	}
}

// ptrBool and ptrString keep the test fixtures readable.
func ptrBool(v bool) *bool       { return &v }
func ptrString(v string) *string { return &v }

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

// fullyPopulatedModel returns a Model that exercises every non-trivial spec
// and status path. Used as the canonical test fixture so round-trip and
// deep-copy coverage stays in sync as the type evolves.
func fullyPopulatedModel() *Model {
	return &Model{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "inference.llmkube.dev/v1alpha1",
			Kind:       "Model",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "qwen3-coder",
			Namespace: "platform",
		},
		Spec: ModelSpec{
			Source:        "https://huggingface.co/org/repo/resolve/main/model.gguf",
			SHA256:        "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			RefreshPolicy: "OnChange",
			Format:        "gguf",
			Quantization:  "Q4_K_M",
			Hardware: &HardwareSpec{
				Accelerator:    "cuda",
				MemoryBudget:   "24Gi",
				MemoryFraction: ptrFloat64(0.75),
				GPU: &GPUSpec{
					Enabled: true,
					Count:   2,
					Memory:  "16Gi",
					Vendor:  "nvidia",
					Layers:  32,
					Sharding: &GPUShardingSpec{
						Strategy:   "layer",
						LayerSplit: []string{"0-15", "16-31"},
					},
				},
			},
			Resources: &ResourceRequirements{
				CPU:    "4",
				Memory: "32Gi",
			},
			Files:  []string{"model.gguf", "MTP/weights.gguf"},
			Mmproj: "mmproj-F16.gguf",
		},
		Status: ModelStatus{
			Phase:               "Ready",
			Size:                "4.2Gi",
			Path:                "/mnt/models/qwen3-coder.gguf",
			CacheKey:            "a1b2c3",
			SHA256:              "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			StagedFiles:         []string{"model.gguf", "MTP/weights.gguf", "mmproj-F16.gguf"},
			SourceETag:          `"abc123"`,
			SourceContentLength: 4509715660,
			AcceleratorReady:    true,
			GGUF: &GGUFMetadata{
				Architecture:  "llama",
				ModelName:     "Qwen3-Coder-7B",
				Quantization:  "Q4_K_M",
				ContextLength: 32768,
				EmbeddingSize: 4096,
				LayerCount:    32,
				HeadCount:     32,
				TensorCount:   128,
				FileVersion:   3,
				License:       "Apache-2.0",
			},
			Conditions: []metav1.Condition{
				{Type: "Available", Status: metav1.ConditionTrue, Reason: "Ready"},
			},
		},
	}
}

// TestModelDeepCopyIndependence verifies the generated DeepCopy methods
// produce a fully independent clone: mutating slices, maps, and pointer
// fields on the copy must not be visible on the original.
func TestModelDeepCopyIndependence(t *testing.T) {
	orig := fullyPopulatedModel()
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

	// Mutate pointer fields on the clone.
	clone.Spec.Hardware.GPU.Layers = 0
	clone.Spec.Hardware.GPU.Sharding.Strategy = "tensor"
	clone.Spec.Hardware.GPU.Sharding.LayerSplit[0] = "MUTATED"

	if got := orig.Spec.Hardware.GPU.Layers; got != 32 {
		t.Errorf("original Spec.Hardware.GPU.Layers = %d; want 32", got)
	}
	if got := orig.Spec.Hardware.GPU.Sharding.Strategy; got != "layer" {
		t.Errorf("original Spec.Hardware.GPU.Sharding.Strategy = %q; want %q", got, "layer")
	}
	if got := orig.Spec.Hardware.GPU.Sharding.LayerSplit[0]; got != "0-15" {
		t.Errorf("original Spec.Hardware.GPU.Sharding.LayerSplit[0] = %q; want %q", got, "0-15")
	}
}

// TestModelJSONRoundTrip confirms every field marshals and unmarshals
// through JSON without loss. This catches missing or incorrect json tags.
func TestModelJSONRoundTrip(t *testing.T) {
	orig := fullyPopulatedModel()

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got Model
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(orig, &got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", orig, &got)
	}
}

// TestModelListDeepCopy verifies the list type's DeepCopy is independent.
func TestModelListDeepCopy(t *testing.T) {
	list := &ModelList{
		Items: []Model{
			*fullyPopulatedModel(),
		},
	}
	clone := list.DeepCopy()

	if !reflect.DeepEqual(list, clone) {
		t.Fatal("ModelList clone differs from original")
	}

	clone.Items[0].Spec.Source = "MUTATED"
	if got := list.Items[0].Spec.Source; got != "https://huggingface.co/org/repo/resolve/main/model.gguf" {
		t.Errorf("original Items[0].Spec.Source = %q; want the original URL", got)
	}
}

// TestModelMinimalSpec verifies a minimal Model (only required fields)
// marshals and unmarshals correctly.
func TestModelMinimalSpec(t *testing.T) {
	m := &Model{
		Spec: ModelSpec{
			Source: "https://example.com/model.gguf",
		},
	}
	data, err := json.Marshal(m.Spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ModelSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Source != "https://example.com/model.gguf" {
		t.Errorf("Source = %q; want %q", got.Source, "https://example.com/model.gguf")
	}
}

// TestModelSchemeRegistration confirms the Model and ModelList types
// are registered with the package's SchemeBuilder (i.e. init() ran).
func TestModelSchemeRegistration(t *testing.T) {
	scheme, err := SchemeBuilder.Build()
	if err != nil {
		t.Fatalf("SchemeBuilder.Build: %v", err)
	}
	gvks, _, err := scheme.ObjectKinds(&Model{})
	if err != nil {
		t.Fatalf("scheme.ObjectKinds(Model): %v", err)
	}
	if len(gvks) == 0 {
		t.Fatal("Model not registered in scheme")
	}
	if gvks[0].Group != "inference.llmkube.dev" || gvks[0].Version != "v1alpha1" || gvks[0].Kind != "Model" {
		t.Errorf("unexpected GVK %s", gvks[0])
	}
}

// TestGPUSpecResourceClaims verifies the ResourceClaims slice round-trips
// through JSON and DeepCopy correctly.
func TestGPUSpecResourceClaims(t *testing.T) {
	spec := GPUSpec{
		Enabled: true,
		Count:   1,
		ResourceClaims: []corev1.PodResourceClaim{
			{ResourceClaimName: ptrString("gpu-claim")},
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GPUSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ResourceClaims[0].ResourceClaimName == nil || *got.ResourceClaims[0].ResourceClaimName != "gpu-claim" {
		t.Errorf("ResourceClaims[0].ResourceClaimName = %v; want %q",
			got.ResourceClaims[0].ResourceClaimName, "gpu-claim")
	}
}

// TestHardwareSpecMemoryFraction verifies the MemoryFraction pointer field
// round-trips correctly through JSON.
func TestHardwareSpecMemoryFraction(t *testing.T) {
	spec := HardwareSpec{
		Accelerator:    "cuda",
		MemoryFraction: ptrFloat64(0.5),
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got HardwareSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.MemoryFraction == nil || *got.MemoryFraction != 0.5 {
		t.Errorf("MemoryFraction = %v; want 0.5", got.MemoryFraction)
	}
}

// TestGGUFMetadataRoundTrip verifies GGUF metadata round-trips through JSON.
func TestGGUFMetadataRoundTrip(t *testing.T) {
	meta := &GGUFMetadata{
		Architecture:  "llama",
		ModelName:     "Llama-3.2-3B",
		Quantization:  "Q5_K_M",
		ContextLength: 131072,
		EmbeddingSize: 4096,
		LayerCount:    32,
		HeadCount:     32,
		TensorCount:   256,
		FileVersion:   3,
		License:       "Apache-2.0",
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GGUFMetadata
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(meta, &got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", meta, &got)
	}
}

// TestModelStatusPhaseEnum verifies the documented phase values are
// accepted by the type.
func TestModelStatusPhaseEnum(t *testing.T) {
	phases := []string{"Pending", "Downloading", "Copying", "Ready", "Failed"}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			m := &Model{
				Status: ModelStatus{Phase: phase},
			}
			data, err := json.Marshal(m.Status)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got ModelStatus
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Phase != phase {
				t.Errorf("Phase = %q; want %q", got.Phase, phase)
			}
		})
	}
}

// TestModelRefreshPolicyEnum verifies the documented refresh policy values
// are accepted by the type.
func TestModelRefreshPolicyEnum(t *testing.T) {
	policies := []string{"IfNotPresent", "OnChange"}
	for _, policy := range policies {
		t.Run(policy, func(t *testing.T) {
			m := &Model{
				Spec: ModelSpec{
					Source:        "https://example.com/model.gguf",
					RefreshPolicy: policy,
				},
			}
			data, err := json.Marshal(m.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got ModelSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.RefreshPolicy != policy {
				t.Errorf("RefreshPolicy = %q; want %q", got.RefreshPolicy, policy)
			}
		})
	}
}

// TestModelFormatEnum verifies the documented format values are accepted
// by the type.
func TestModelFormatEnum(t *testing.T) {
	formats := []string{"gguf", "mlx", "safetensors", "pytorch", "custom"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			m := &Model{
				Spec: ModelSpec{
					Source: "https://example.com/model.gguf",
					Format: format,
				},
			}
			data, err := json.Marshal(m.Spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got ModelSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Format != format {
				t.Errorf("Format = %q; want %q", got.Format, format)
			}
		})
	}
}

// TestHardwareAcceleratorEnum verifies the documented accelerator values
// are accepted by the type.
func TestHardwareAcceleratorEnum(t *testing.T) {
	accelerators := []string{"cpu", "metal", "cuda", "rocm", "intel", "vulkan"}
	for _, acc := range accelerators {
		t.Run(acc, func(t *testing.T) {
			hw := HardwareSpec{Accelerator: acc}
			data, err := json.Marshal(hw)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got HardwareSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Accelerator != acc {
				t.Errorf("Accelerator = %q; want %q", got.Accelerator, acc)
			}
		})
	}
}

// TestGPUVendorEnum verifies the documented GPU vendor values are accepted
// by the type.
func TestGPUVendorEnum(t *testing.T) {
	vendors := []string{"nvidia", "amd", "intel"}
	for _, vendor := range vendors {
		t.Run(vendor, func(t *testing.T) {
			gpu := GPUSpec{Vendor: vendor}
			data, err := json.Marshal(gpu)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got GPUSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Vendor != vendor {
				t.Errorf("Vendor = %q; want %q", got.Vendor, vendor)
			}
		})
	}
}

// TestGPUShardingStrategyEnum verifies the documented sharding strategy
// values are accepted by the type.
func TestGPUShardingStrategyEnum(t *testing.T) {
	strategies := []string{"layer", "tensor", "row", "pipeline", "none"}
	for _, strategy := range strategies {
		t.Run(strategy, func(t *testing.T) {
			shard := GPUShardingSpec{Strategy: strategy}
			data, err := json.Marshal(shard)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got GPUShardingSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Strategy != strategy {
				t.Errorf("Strategy = %q; want %q", got.Strategy, strategy)
			}
		})
	}
}

// TestGPUResourceName verifies the ResourceName and TolerationKey fields
// round-trip correctly through JSON.
func TestGPUResourceName(t *testing.T) {
	gpu := GPUSpec{
		ResourceName:  "devic.es/dri-render",
		TolerationKey: "nvidia.com/gpu",
	}
	data, err := json.Marshal(gpu)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got GPUSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ResourceName != "devic.es/dri-render" {
		t.Errorf("ResourceName = %q; want %q", got.ResourceName, "devic.es/dri-render")
	}
	if got.TolerationKey != "nvidia.com/gpu" {
		t.Errorf("TolerationKey = %q; want %q", got.TolerationKey, "nvidia.com/gpu")
	}
}

// TestGPURuntimeEnum verifies the documented GPU runtime values are
// accepted by the type.
func TestGPURuntimeEnum(t *testing.T) {
	runtimes := []string{"vulkan", "rocm"}
	for _, rt := range runtimes {
		t.Run(rt, func(t *testing.T) {
			gpu := GPUSpec{Runtime: rt}
			data, err := json.Marshal(gpu)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got GPUSpec
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Runtime != rt {
				t.Errorf("Runtime = %q; want %q", got.Runtime, rt)
			}
		})
	}
}

// TestResourceRequirementsRoundTrip verifies ResourceRequirements
// round-trips through JSON.
func TestResourceRequirementsRoundTrip(t *testing.T) {
	res := ResourceRequirements{
		CPU:    "4",
		Memory: "32Gi",
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ResourceRequirements
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(res, got) {
		t.Fatalf("round-trip mismatch.\noriginal: %#v\ndecoded:  %#v", res, got)
	}
}

// TestModelFilesAndMmprojRoundTrip verifies the multi-file staging fields
// round-trip through JSON correctly.
func TestModelFilesAndMmprojRoundTrip(t *testing.T) {
	model := &Model{
		Spec: ModelSpec{
			Source: "hf://unsloth/gemma-4-31B-it-GGUF",
			Files: []string{
				"gemma-4-31B-it-UD-Q4_K_XL.gguf",
				"MTP/gemma-4-31B-it-Q8_0-MTP.gguf",
			},
			Mmproj: "mmproj-F16.gguf",
			Format: "gguf",
		},
		Status: ModelStatus{
			StagedFiles: []string{
				"gemma-4-31B-it-UD-Q4_K_XL.gguf",
				"MTP/gemma-4-31B-it-Q8_0-MTP.gguf",
				"mmproj-F16.gguf",
			},
		},
	}

	data, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got Model
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if !reflect.DeepEqual(got.Spec.Files, model.Spec.Files) {
		t.Fatalf("Spec.Files = %#v; want %#v", got.Spec.Files, model.Spec.Files)
	}
	if got.Spec.Mmproj != model.Spec.Mmproj {
		t.Fatalf("Spec.Mmproj = %q; want %q", got.Spec.Mmproj, model.Spec.Mmproj)
	}
	if !reflect.DeepEqual(got.Status.StagedFiles, model.Status.StagedFiles) {
		t.Fatalf("Status.StagedFiles = %#v; want %#v", got.Status.StagedFiles, model.Status.StagedFiles)
	}
}

// ptrFloat64 keeps the test fixtures readable.
func ptrFloat64(v float64) *float64 { return &v }

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

package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadCatalog(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	if catalog == nil {
		t.Fatal("Catalog is nil")
		return
	}

	if catalog.Version == "" {
		t.Error("Catalog version is empty")
	}

	if len(catalog.Models) == 0 {
		t.Error("Catalog has no models")
	}

	// Verify we have the expected number of models
	expectedModelCount := 17
	if len(catalog.Models) != expectedModelCount {
		t.Errorf("Expected %d models, got %d", expectedModelCount, len(catalog.Models))
	}
}

func TestLoadCatalogCaching(t *testing.T) {
	// Load catalog twice to test caching
	catalog1, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog first time: %v", err)
	}

	catalog2, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog second time: %v", err)
	}

	// Should be the same instance due to caching
	if catalog1 != catalog2 {
		t.Error("Catalog instances are different - caching not working")
	}
}

func TestGetModel(t *testing.T) {
	testCases := []struct {
		name        string
		modelID     string
		shouldExist bool
	}{
		{"Llama 3.1 8B exists", "llama-3.1-8b", true},
		{"Mistral 7B exists", "mistral-7b", true},
		{"Qwen Coder exists", "qwen-2.5-coder-7b", true},
		{"DeepSeek R1 7B exists", "deepseek-r1-7b", true},
		{"Phi-4 Mini exists", "phi-4-mini", true},
		{"Gemma 3 12B exists", "gemma-3-12b", true},
		{"Qwen 14B exists", "qwen-2.5-14b", true},
		{"Mixtral exists", "mixtral-8x7b", true},
		{"Llama 70B exists", "llama-3.3-70b", true},
		{"Llama 3.2 3B exists", "llama-3.2-3b", true},
		{"Qwen 2.5 32B exists", "qwen-2.5-32b", true},
		{"Qwen 2.5 Coder 32B exists", "qwen-2.5-coder-32b", true},
		{"Qwen 3 32B exists", "qwen-3-32b", true},
		{"DeepSeek R1 14B exists", "deepseek-r1-14b", true},
		{"DeepSeek R1 32B exists", "deepseek-r1-32b", true},
		{"Mistral Small 24B exists", "mistral-small-24b", true},
		{"Gemma 3 4B exists", "gemma-3-4b", true},
		{"Non-existent model", "non-existent-model", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			model, err := GetModel(tc.modelID)

			if tc.shouldExist {
				if err != nil {
					t.Errorf("Expected model '%s' to exist, but got error: %v", tc.modelID, err)
				}
				if model == nil {
					t.Errorf("Expected model '%s' to be non-nil", tc.modelID)
				}
			} else {
				if err == nil {
					t.Errorf("Expected error for non-existent model '%s', but got none", tc.modelID)
				}
				if model != nil {
					t.Errorf("Expected nil model for '%s', but got model: %+v", tc.modelID, model)
				}
			}
		})
	}
}

func TestModelFields(t *testing.T) {
	model, err := GetModel("llama-3.1-8b")
	if err != nil {
		t.Fatalf("Failed to get llama-3.1-8b model: %v", err)
	}

	// Test required fields are present
	if model.Name == "" {
		t.Error("Model name is empty")
	}

	if model.Description == "" {
		t.Error("Model description is empty")
	}

	if model.Size == "" {
		t.Error("Model size is empty")
	}

	if model.Source == "" {
		t.Error("Model source is empty")
	}

	if model.Quantization == "" {
		t.Error("Model quantization is empty")
	}

	if model.VRAMEstimate == "" {
		t.Error("Model VRAM estimate is empty")
	}

	if model.Homepage == "" {
		t.Error("Model homepage is empty")
	}

	// Test numeric fields have valid values
	if model.ContextSize <= 0 {
		t.Errorf("Expected positive context size, got %d", model.ContextSize)
	}

	if model.GPULayers <= 0 {
		t.Errorf("Expected positive GPU layers, got %d", model.GPULayers)
	}

	// Test resource fields
	if model.Resources.CPU == "" {
		t.Error("Model CPU resource is empty")
	}

	if model.Resources.Memory == "" {
		t.Error("Model memory resource is empty")
	}

	if model.Resources.GPUMemory == "" {
		t.Error("Model GPU memory resource is empty")
	}

	// Test arrays are populated
	if len(model.UseCases) == 0 {
		t.Error("Model has no use cases")
	}

	if len(model.Tags) == 0 {
		t.Error("Model has no tags")
	}
}

func TestAllModelsHaveValidFields(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	for modelID, model := range catalog.Models {
		t.Run(modelID, func(t *testing.T) {
			// Verify required fields
			requiredFields := map[string]string{
				"name":         model.Name,
				"description":  model.Description,
				"size":         model.Size,
				"source":       model.Source,
				"quantization": model.Quantization,
				"vram":         model.VRAMEstimate,
				"homepage":     model.Homepage,
				"cpu":          model.Resources.CPU,
				"memory":       model.Resources.Memory,
				"gpu_memory":   model.Resources.GPUMemory,
			}

			for fieldName, fieldValue := range requiredFields {
				if fieldValue == "" {
					t.Errorf("Model '%s' has empty %s field", modelID, fieldName)
				}
			}

			// Verify numeric fields
			if model.ContextSize <= 0 {
				t.Errorf("Model '%s' has invalid context size: %d", modelID, model.ContextSize)
			}

			if model.GPULayers <= 0 {
				t.Errorf("Model '%s' has invalid GPU layers: %d", modelID, model.GPULayers)
			}

			// Verify arrays
			if len(model.UseCases) == 0 {
				t.Errorf("Model '%s' has no use cases", modelID)
			}

			if len(model.Tags) == 0 {
				t.Errorf("Model '%s' has no tags", modelID)
			}
		})
	}
}

func TestModelTagFiltering(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	testCases := []struct {
		tag              string
		expectedMinCount int
	}{
		{"code", 2},        // Should have at least 2 code-related models
		{"recommended", 2}, // Should have at least 2 recommended models
		{"llama", 3},       // Should have at least 3 Llama models
		{"small", 2},       // Should have at least 2 small models
	}

	for _, tc := range testCases {
		t.Run(tc.tag, func(t *testing.T) {
			count := 0
			for _, model := range catalog.Models {
				if containsTag(model.Tags, tc.tag) {
					count++
				}
			}

			if count < tc.expectedMinCount {
				t.Errorf("Expected at least %d models with tag '%s', got %d",
					tc.expectedMinCount, tc.tag, count)
			}
		})
	}
}

func TestContainsTag(t *testing.T) {
	testCases := []struct {
		name     string
		tags     []string
		tag      string
		expected bool
	}{
		{"exact match", []string{"code", "llama", "recommended"}, "code", true},
		{"case insensitive", []string{"Code", "LLAMA"}, "code", true},
		{"not present", []string{"code", "llama"}, "mistral", false},
		{"empty tags", []string{}, "code", false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := containsTag(tc.tags, tc.tag)
			if result != tc.expected {
				t.Errorf("containsTag(%v, %s) = %v, expected %v",
					tc.tags, tc.tag, result, tc.expected)
			}
		})
	}
}

func TestFormatUseCase(t *testing.T) {
	testCases := []struct {
		input    string
		expected string
	}{
		{"code-generation", "Code Generation"},
		{"general-purpose", "General Purpose"},
		{"chat", "Chat"},
		{"multilingual", "Multilingual"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			result := formatUseCase(tc.input)
			if result != tc.expected {
				t.Errorf("formatUseCase(%s) = %s, expected %s",
					tc.input, result, tc.expected)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"no truncation needed", "Hello", 10, "Hello"},
		{"exact length", "Hello", 5, "Hello"},
		{"truncation needed", "Hello World", 8, "Hello..."},
		{"very short", "Hello World", 3, "..."},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := truncate(tc.input, tc.maxLen)
			if result != tc.expected {
				t.Errorf("truncate(%s, %d) = %s, expected %s",
					tc.input, tc.maxLen, result, tc.expected)
			}
		})
	}
}

func TestFormatNumber(t *testing.T) {
	testCases := []struct {
		input    int
		expected string
	}{
		{100, "100"},
		{1000, "1,000"},
		{1234, "1,234"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
		{8192, "8,192"},
		{131072, "131,072"},
	}

	for _, tc := range testCases {
		t.Run(tc.expected, func(t *testing.T) {
			result := formatNumber(tc.input)
			if result != tc.expected {
				t.Errorf("formatNumber(%d) = %s, expected %s",
					tc.input, result, tc.expected)
			}
		})
	}
}

func TestModelQuantizationFormats(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	validQuantizations := map[string]bool{
		"Q4_K_M": true,
		"Q5_K_M": true,
		"Q8_0":   true,
	}

	for modelID, model := range catalog.Models {
		if model.Runtime != "llamacpp" {
			if model.Quantization == "" {
				t.Errorf("Model %s needs quantization metadata", modelID)
			}
			continue
		}
		if !validQuantizations[model.Quantization] {
			t.Errorf("Model '%s' has unexpected quantization format: %s",
				modelID, model.Quantization)
		}
	}
}

func TestModelSourceURLs(t *testing.T) {
	catalog, err := LoadCatalog()
	if err != nil {
		t.Fatalf("Failed to load catalog: %v", err)
	}

	for modelID, model := range catalog.Models {
		if model.Runtime != "llamacpp" {
			if len(model.Files) == 0 {
				t.Errorf("Model %s needs a file set", modelID)
			}
			if model.Validated == nil || model.Validated.Hardware == "" || model.Validated.LLMKube == "" {
				t.Errorf("Model %s needs hardware and LLMKube validation metadata", modelID)
			}
			continue
		}
		// Verify source URLs are HuggingFace URLs
		if !strings.HasPrefix(model.Source, "https://huggingface.co/") {
			t.Errorf("Model '%s' source URL should start with https://huggingface.co/, got: %s",
				modelID, model.Source)
		}

		// Verify source URLs end with .gguf
		if !strings.HasSuffix(model.Source, ".gguf") {
			t.Errorf("Model '%s' source URL should end with .gguf, got: %s",
				modelID, model.Source)
		}
	}
}

func TestRecommendedModelsExist(t *testing.T) {
	// These are the models we want to ensure are always in the catalog
	// as they're highlighted in documentation
	recommendedModels := []string{
		"llama-3.1-8b",
		"qwen-2.5-coder-7b",
		"mistral-7b",
	}

	for _, modelID := range recommendedModels {
		t.Run(modelID, func(t *testing.T) {
			model, err := GetModel(modelID)
			if err != nil {
				t.Errorf("Recommended model '%s' not found in catalog: %v", modelID, err)
			}
			if model == nil {
				t.Errorf("Recommended model '%s' is nil", modelID)
			}
		})
	}
}

func TestRunCatalogList(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogList("")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogList error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "RUNTIME") || !strings.Contains(output, "llamacpp") {
		t.Error("output must show runtimes")
	}
	if !strings.Contains(output, "Model Catalog") {
		t.Error("output should contain catalog header")
	}
	if !strings.Contains(output, "llama") {
		t.Error("output should list llama models")
	}
	if !strings.Contains(output, "To deploy") {
		t.Error("output should contain deploy instructions")
	}
}

func TestRunCatalogListWithTag(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogList("code")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogList(code) error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "tag=code") {
		t.Error("output should show active tag filter")
	}
}

func TestRunCatalogListWithNonexistentTag(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogList("nonexistent-tag-xyz")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogList(nonexistent) error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No models found") {
		t.Error("output should indicate no models found for tag")
	}
}

func TestRunCatalogInfo(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogInfo("llama-3.1-8b")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogInfo error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "llama-3.1-8b") {
		t.Error("output should contain model ID")
	}
	if !strings.Contains(output, "Quick Deploy") {
		t.Error("output should contain deploy instructions")
	}
	if !strings.Contains(output, "Resource Requirements") {
		t.Error("output should contain resource requirements")
	}
	for _, want := range []string{"Context Size:    8,192 tokens", "VRAM Estimate:   7-8GB"} {
		if !strings.Contains(output, want) {
			t.Errorf("catalog info missing %q", want)
		}
	}
}

func TestRunCatalogInfoQwen14BContext(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogInfo("qwen-2.5-14b")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogInfo error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	for _, want := range []string{"Context Size:    16,384 tokens", "VRAM Estimate:   14-16GB"} {
		if !strings.Contains(output, want) {
			t.Errorf("catalog info missing %q", want)
		}
	}
}

func TestRunCatalogInfoQwenCoder7BContext(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogInfo("qwen-2.5-coder-7b")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogInfo error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	// The model's trained window is 32,768 (max_position_embeddings, no rope_scaling);
	// asking llama-server for 131,072 would run it past the context it was trained on.
	for _, want := range []string{"Context Size:    32,768 tokens", "VRAM Estimate:   7-8GB"} {
		if !strings.Contains(output, want) {
			t.Errorf("catalog info missing %q", want)
		}
	}
}

func TestRunCatalogInfoLlama70BContext(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runCatalogInfo("llama-3.3-70b")

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("runCatalogInfo error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	for _, want := range []string{"Context Size:    8,192 tokens", "VRAM Estimate:   45-55GB"} {
		if !strings.Contains(output, want) {
			t.Errorf("catalog info missing %q", want)
		}
	}
}

func TestRunCatalogInfoNonexistent(t *testing.T) {
	err := runCatalogInfo("nonexistent-model-xyz")
	if err == nil {
		t.Error("runCatalogInfo should error for non-existent model")
	}
}

func TestNewCatalogCommand(t *testing.T) {
	cmd := NewCatalogCommand()

	if cmd.Use != "catalog" {
		t.Errorf("Use = %q, want %q", cmd.Use, "catalog")
	}

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["list"] {
		t.Error("Missing 'list' subcommand")
	}
	if !subcommands["info"] {
		t.Error("Missing 'info' subcommand")
	}
}

func TestNewCatalogListCommand(t *testing.T) {
	cmd := NewCatalogListCommand()

	if cmd.Use != "list" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list")
	}
	if cmd.Flags().Lookup("tag") == nil {
		t.Error("Missing --tag flag")
	}
}

func TestNewCatalogInfoCommand(t *testing.T) {
	cmd := NewCatalogInfoCommand()

	if cmd.Use != "info MODEL_ID" {
		t.Errorf("Use = %q, want %q", cmd.Use, "info MODEL_ID")
	}
}

func TestCatalogRuntimeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		valid       bool
	}{
		{"legacy", "source: https://example.com/model.gguf\ngpu_layers: 0", true},
		{"vllm", "runtime: vllm\nfiles: [weights.safetensors]\ngpu: {count: 1}", true},
		{"sglang", "runtime: sglang\nfiles: [weights.safetensors]\ngpu: {count: 1}", true},
		{"vllm missing GPUs", "runtime: vllm\nfiles: [weights.safetensors]", false},
		{"sglang missing GPUs", "runtime: sglang\nfiles: [weights.safetensors]", false},
		{"vllm zero GPUs", "runtime: vllm\nfiles: [weights.safetensors]\ngpu: {count: 0}", false},
		{"sglang zero GPUs", "runtime: sglang\nfiles: [weights.safetensors]\ngpu: {count: 0}", false},
		{"malformed GPU count", "gpu: {count: nope}", false},
		{"unknown", "runtime: typo", false},
		{"llamacpp parsers", "parsers: {tool_call: hermes}", false},
		{"llamacpp served name", "served_model_name: chat", false},
		{"llamacpp tensor parallel", "gpu: {count: 2, tensor_parallel_size: 2}", false},
		{"vllm layers zero", "runtime: vllm\nfiles: [weights.safetensors]\ngpu_layers: 0", false},
		{"sglang layers", "runtime: sglang\nfiles: [weights.safetensors]\ngpu_layers: -1", false},
		{"missing files", "runtime: sglang\ngpu: {count: 1}", false},
		{"zero GPUs", "gpu: {count: 0}", false},
		{"too many shards", "runtime: vllm\nfiles: [weights.safetensors]\ngpu: {count: 1, tensor_parallel_size: 2}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var catalog Catalog
			entry := "version: test\nmodels:\n  test:\n    " + strings.ReplaceAll(tc.entry, "\n", "\n    ")
			err := yaml.Unmarshal([]byte(entry), &catalog)
			if (err == nil) != tc.valid {
				t.Fatalf("parse error = %v, valid = %v", err, tc.valid)
			}
			if err != nil && !strings.Contains(err.Error(), "line 4:") {
				t.Errorf("catalog error must identify the entry line: %v", err)
			}
			if tc.name == "legacy" && catalog.Models["test"].Runtime != "llamacpp" {
				t.Fatal("legacy runtime must default to llamacpp")
			}
		})
	}
}

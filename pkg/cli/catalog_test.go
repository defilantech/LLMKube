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

package cli

import (
	"strings"
	"testing"
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
	expectedModelCount := 13
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
		{"DeepSeek Coder exists", "deepseek-coder-6.7b", true},
		{"Phi-3 Mini exists", "phi-3-mini", true},
		{"Gemma 2 9B exists", "gemma-2-9b", true},
		{"Qwen 14B exists", "qwen-2.5-14b", true},
		{"Mixtral exists", "mixtral-8x7b", true},
		{"Llama 70B exists", "llama-3.1-70b", true},
		{"Llama 3.2 3B exists", "llama-3.2-3b", true},
		{"Qwen 2.5 32B exists", "qwen-2.5-32b", true},
		{"Qwen 2.5 Coder 32B exists", "qwen-2.5-coder-32b", true},
		{"Qwen 3 32B exists", "qwen-3-32b", true},
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

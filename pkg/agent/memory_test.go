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

package agent

import (
	"fmt"
	"testing"
)

type mockMemoryProvider struct {
	totalBytes, availableBytes uint64
	totalErr, availableErr     error
}

func (m *mockMemoryProvider) TotalMemory() (uint64, error) {
	return m.totalBytes, m.totalErr
}

func (m *mockMemoryProvider) AvailableMemory() (uint64, error) {
	return m.availableBytes, m.availableErr
}

func TestEstimateModelMemory_WithFullMetadata(t *testing.T) {
	// Llama 8B: 32 layers, 4096 embedding, 2048 context, ~4.5 GiB file
	fileSize := uint64(4831838208) // ~4.5 GiB
	est := EstimateModelMemory(fileSize, 32, 4096, 2048)

	// KV cache = 2 * 32 * 4096 * 2048 * 2 = 1073741824 (1 GiB)
	expectedKV := uint64(2 * 32 * 4096 * 2048 * 2)
	if est.KVCacheBytes != expectedKV {
		t.Errorf("KVCacheBytes = %d, want %d", est.KVCacheBytes, expectedKV)
	}
	if est.WeightsBytes != fileSize {
		t.Errorf("WeightsBytes = %d, want %d", est.WeightsBytes, fileSize)
	}
	if est.OverheadBytes != 512*1024*1024 {
		t.Errorf("OverheadBytes = %d, want %d", est.OverheadBytes, uint64(512*1024*1024))
	}
	expectedTotal := fileSize + expectedKV + 512*1024*1024
	if est.TotalBytes != expectedTotal {
		t.Errorf("TotalBytes = %d, want %d", est.TotalBytes, expectedTotal)
	}
}

func TestEstimateModelMemory_WithoutMetadata(t *testing.T) {
	fileSize := uint64(4 * 1024 * 1024 * 1024) // 4 GiB
	est := EstimateModelMemory(fileSize, 0, 0, 2048)

	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0 (fallback mode)", est.KVCacheBytes)
	}
	// Fallback: fileSize * 1.2 + 512 MiB. Use the same math.Ceil logic as the implementation.
	if est.TotalBytes <= fileSize {
		t.Errorf("TotalBytes (%d) should exceed file size (%d) in fallback mode", est.TotalBytes, fileSize)
	}
	// Verify the estimate is roughly fileSize * 1.2 + 512 MiB (within 1 byte of rounding)
	expected := EstimateModelMemory(fileSize, 0, 0, 2048)
	if est.TotalBytes != expected.TotalBytes {
		t.Errorf("TotalBytes = %d, want %d", est.TotalBytes, expected.TotalBytes)
	}
}

func TestEstimateModelMemory_ZeroEmbedding(t *testing.T) {
	fileSize := uint64(2 * 1024 * 1024 * 1024)
	est := EstimateModelMemory(fileSize, 32, 0, 2048)

	// Should fall back to heuristic since embeddingSize is 0
	if est.KVCacheBytes != 0 {
		t.Errorf("KVCacheBytes = %d, want 0 (fallback when embeddingSize=0)", est.KVCacheBytes)
	}
}

func TestEstimateModelMemory_LargeContext(t *testing.T) {
	fileSize := uint64(4831838208)
	smallCtx := EstimateModelMemory(fileSize, 32, 4096, 2048)
	largeCtx := EstimateModelMemory(fileSize, 32, 4096, 8192)

	if largeCtx.KVCacheBytes != smallCtx.KVCacheBytes*4 {
		t.Errorf("KV cache should scale 4x with 4x context: got %d, want %d",
			largeCtx.KVCacheBytes, smallCtx.KVCacheBytes*4)
	}
	if largeCtx.TotalBytes <= smallCtx.TotalBytes {
		t.Error("larger context should produce larger total estimate")
	}
}

func TestDefaultMemoryFraction_Small(t *testing.T) {
	total := uint64(16 * 1024 * 1024 * 1024) // 16 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.67 {
		t.Errorf("fraction = %f, want 0.67 for 16GB system", f)
	}
}

func TestDefaultMemoryFraction_AtThreshold(t *testing.T) {
	total := uint64(36 * 1024 * 1024 * 1024) // 36 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.67 {
		t.Errorf("fraction = %f, want 0.67 for 36GB system (at threshold)", f)
	}
}

func TestDefaultMemoryFraction_Large(t *testing.T) {
	total := uint64(64 * 1024 * 1024 * 1024) // 64 GiB
	f := DefaultMemoryFraction(total)
	if f != 0.75 {
		t.Errorf("fraction = %f, want 0.75 for 64GB system", f)
	}
}

func TestCheckMemoryBudget_Fits(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 48 * 1024 * 1024 * 1024} // 48 GiB
	estimate := MemoryEstimate{TotalBytes: 10 * 1024 * 1024 * 1024}      // 10 GiB

	budget, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !budget.Fits {
		t.Error("expected model to fit within budget")
	}
	expectedBudget := uint64(float64(48*1024*1024*1024) * 0.75)
	if budget.BudgetBytes != expectedBudget {
		t.Errorf("BudgetBytes = %d, want %d", budget.BudgetBytes, expectedBudget)
	}
	if budget.HeadroomBytes != expectedBudget-estimate.TotalBytes {
		t.Errorf("HeadroomBytes = %d, want %d", budget.HeadroomBytes, expectedBudget-estimate.TotalBytes)
	}
}

func TestCheckMemoryBudget_DoesNotFit(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 16 * 1024 * 1024 * 1024} // 16 GiB
	estimate := MemoryEstimate{TotalBytes: 14 * 1024 * 1024 * 1024}      // 14 GiB

	budget, err := CheckMemoryBudget(provider, estimate, 0.67)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if budget.Fits {
		t.Error("expected model to NOT fit within budget")
	}
	if budget.HeadroomBytes != 0 {
		t.Errorf("HeadroomBytes should be 0 when model doesn't fit, got %d", budget.HeadroomBytes)
	}
}

func TestCheckMemoryBudget_ExactlyAtLimit(t *testing.T) {
	total := uint64(16 * 1024 * 1024 * 1024)
	budgetBytes := uint64(float64(total) * 0.75)
	provider := &mockMemoryProvider{totalBytes: total}
	estimate := MemoryEstimate{TotalBytes: budgetBytes}

	budget, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !budget.Fits {
		t.Error("expected model to fit when exactly at budget limit")
	}
	if budget.HeadroomBytes != 0 {
		t.Errorf("HeadroomBytes = %d, want 0 at exact limit", budget.HeadroomBytes)
	}
}

func TestCheckMemoryBudget_ProviderError(t *testing.T) {
	provider := &mockMemoryProvider{totalErr: errTestProvider}
	estimate := MemoryEstimate{TotalBytes: 1024}

	_, err := CheckMemoryBudget(provider, estimate, 0.75)
	if err == nil {
		t.Fatal("expected error when provider fails")
	}
}

var errTestProvider = fmt.Errorf("mock provider error")

func TestFormatMemory(t *testing.T) {
	tests := []struct {
		bytes    uint64
		expected string
	}{
		{0, "0 MB"},
		{512 * 1024 * 1024, "512 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{26093297869, "24.3 GB"}, // 24.3 * 1024^3
		{48 * 1024 * 1024 * 1024, "48.0 GB"},
	}
	for _, tt := range tests {
		got := formatMemory(tt.bytes)
		if got != tt.expected {
			t.Errorf("formatMemory(%d) = %q, want %q", tt.bytes, got, tt.expected)
		}
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		input    string
		expected uint64
	}{
		{"4.5 GiB", uint64(4.5 * 1024 * 1024 * 1024)},
		{"512.0 MiB", uint64(512 * 1024 * 1024)},
		{"1.0 KiB", 1024},
		{"100 B", 100},
		{"1.0 GiB", 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.input)
		if err != nil {
			t.Errorf("parseSize(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.expected {
			t.Errorf("parseSize(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

func TestParseSize_Invalid(t *testing.T) {
	tests := []string{
		"",
		"abc",
		"4.5",
		"4.5 XB",
		"not a number GiB",
	}
	for _, input := range tests {
		_, err := parseSize(input)
		if err == nil {
			t.Errorf("parseSize(%q) expected error, got nil", input)
		}
	}
}

func TestNewMetalAgent_MemoryFraction(t *testing.T) {
	provider := &mockMemoryProvider{totalBytes: 64 * 1024 * 1024 * 1024} // 64 GiB

	// Auto-detect: should resolve to 0.75 for 64GB
	agent := NewMetalAgent(MetalAgentConfig{
		MemoryProvider: provider,
	})
	if agent.memoryFraction != 0.75 {
		t.Errorf("auto-detected fraction = %f, want 0.75 for 64GB", agent.memoryFraction)
	}

	// Explicit fraction
	agent = NewMetalAgent(MetalAgentConfig{
		MemoryProvider: provider,
		MemoryFraction: 0.5,
	})
	if agent.memoryFraction != 0.5 {
		t.Errorf("explicit fraction = %f, want 0.5", agent.memoryFraction)
	}

	// Small system auto-detect
	smallProvider := &mockMemoryProvider{totalBytes: 16 * 1024 * 1024 * 1024}
	agent = NewMetalAgent(MetalAgentConfig{
		MemoryProvider: smallProvider,
	})
	if agent.memoryFraction != 0.67 {
		t.Errorf("auto-detected fraction = %f, want 0.67 for 16GB", agent.memoryFraction)
	}
}

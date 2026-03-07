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
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// MemoryPressureLevel represents the severity of memory pressure.
type MemoryPressureLevel int

const (
	MemoryPressureNormal   MemoryPressureLevel = 0
	MemoryPressureWarning  MemoryPressureLevel = 1
	MemoryPressureCritical MemoryPressureLevel = 2
)

func (l MemoryPressureLevel) String() string {
	switch l {
	case MemoryPressureWarning:
		return "warning"
	case MemoryPressureCritical:
		return "critical"
	default:
		return "normal"
	}
}

// MemoryProvider abstracts system memory queries for testability.
type MemoryProvider interface {
	TotalMemory() (uint64, error)
	AvailableMemory() (uint64, error)
	WiredMemory() (uint64, error)
	ProcessRSS(pid int) (uint64, error)
}

// MemoryEstimate holds the estimated memory requirements for a model.
type MemoryEstimate struct {
	WeightsBytes  uint64
	KVCacheBytes  uint64
	OverheadBytes uint64
	TotalBytes    uint64
}

// MemoryBudget is the result of a memory budget check.
type MemoryBudget struct {
	Fits          bool
	TotalBytes    uint64
	BudgetBytes   uint64
	EstimateBytes uint64
	HeadroomBytes uint64
}

const overheadBytes = 512 * 1024 * 1024 // 512 MiB constant overhead

// EstimateModelMemory estimates total memory needed to serve a model.
// When GGUF metadata is available (layerCount > 0 and embeddingSize > 0),
// it computes KV cache as 2 * layerCount * embeddingSize * contextSize * 2 (FP16 K+V).
// Otherwise it falls back to fileSize * 1.2 + overhead.
func EstimateModelMemory(fileSizeBytes uint64, layerCount, embeddingSize uint64, contextSize int) MemoryEstimate {
	if layerCount > 0 && embeddingSize > 0 {
		// KV cache: 4 * layers * embedding * context (FP16 K+V)
		// Guard against overflow in the multiplication chain.
		layerEmbed := layerCount * embeddingSize
		if layerEmbed/layerCount != embeddingSize {
			return fallbackEstimate(fileSizeBytes)
		}
		ctx := uint64(contextSize)
		product := layerEmbed * ctx
		if ctx != 0 && product/ctx != layerEmbed {
			return fallbackEstimate(fileSizeBytes)
		}
		kvCache := product * 4
		if kvCache/4 != product {
			return fallbackEstimate(fileSizeBytes)
		}
		total := fileSizeBytes + kvCache + uint64(overheadBytes)
		if total < fileSizeBytes { // addition overflow
			return fallbackEstimate(fileSizeBytes)
		}
		return MemoryEstimate{
			WeightsBytes:  fileSizeBytes,
			KVCacheBytes:  kvCache,
			OverheadBytes: uint64(overheadBytes),
			TotalBytes:    total,
		}
	}

	return fallbackEstimate(fileSizeBytes)
}

// fallbackEstimate uses a heuristic (fileSize * 1.2 + overhead) when GGUF
// metadata is unavailable or the KV cache calculation would overflow.
func fallbackEstimate(fileSizeBytes uint64) MemoryEstimate {
	scaled := uint64(math.Ceil(float64(fileSizeBytes) * 1.2))
	total := scaled + uint64(overheadBytes)
	return MemoryEstimate{
		WeightsBytes:  fileSizeBytes,
		KVCacheBytes:  0,
		OverheadBytes: scaled - fileSizeBytes + uint64(overheadBytes),
		TotalBytes:    total,
	}
}

// DefaultMemoryFraction returns the fraction of total system memory to use
// as a budget for model serving. Returns 0.67 for systems ≤36GB, 0.75 for larger.
func DefaultMemoryFraction(totalBytes uint64) float64 {
	const threshold = 36 * 1024 * 1024 * 1024 // 36 GiB
	if totalBytes <= threshold {
		return 0.67
	}
	return 0.75
}

// CheckMemoryBudget checks whether a model's estimated memory fits within
// the system's memory budget (total * fraction).
func CheckMemoryBudget(provider MemoryProvider, estimate MemoryEstimate, fraction float64) (*MemoryBudget, error) {
	total, err := provider.TotalMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get total memory: %w", err)
	}

	budget := uint64(float64(total) * fraction)
	fits := estimate.TotalBytes <= budget

	var headroom uint64
	if fits {
		headroom = budget - estimate.TotalBytes
	}

	return &MemoryBudget{
		Fits:          fits,
		TotalBytes:    total,
		BudgetBytes:   budget,
		EstimateBytes: estimate.TotalBytes,
		HeadroomBytes: headroom,
	}, nil
}

// formatMemory formats a byte count into a human-readable string like "24.3 GB" or "512 MB".
func formatMemory(bytes uint64) string {
	const (
		gb = 1024 * 1024 * 1024
		mb = 1024 * 1024
	)
	if bytes >= gb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(bytes)/float64(mb))
}

// parseSize parses a human-readable size string (as produced by model_controller's
// formatBytes, e.g. "4.5 GiB", "512.0 MiB", "1024 B") back to bytes.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}

	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid size format %q: expected \"<number> <unit>\"", s)
	}

	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size number %q: %w", parts[0], err)
	}

	var multiplier float64
	switch strings.ToUpper(parts[1]) {
	case "B":
		multiplier = 1
	case "KIB":
		multiplier = 1024
	case "MIB":
		multiplier = 1024 * 1024
	case "GIB":
		multiplier = 1024 * 1024 * 1024
	case "TIB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown size unit %q", parts[1])
	}

	return uint64(value * multiplier), nil
}

const (
	// BudgetModeAbsolute indicates a fixed byte budget from the CRD.
	BudgetModeAbsolute = "absolute"
	// BudgetModeFraction indicates a fraction-of-memory budget.
	BudgetModeFraction = "fraction"
)

// ResolvedBudget describes the effective memory budget after applying the
// precedence chain: CRD absolute → CRD fraction → agent flag → auto-detect.
type ResolvedBudget struct {
	// Mode is BudgetModeAbsolute when a fixed byte budget is in effect, or
	// BudgetModeFraction when the budget is a percentage of total system memory.
	Mode string
	// Bytes is the absolute budget in bytes. Set only when Mode == "absolute".
	Bytes uint64
	// Fraction is the fraction of total memory. Set only when Mode == "fraction".
	Fraction float64
	// Source describes where the value came from for logging.
	Source string
}

// ResolveMemoryBudget implements the precedence chain:
//  1. model.Spec.Hardware.MemoryBudget  (absolute byte limit)
//  2. model.Spec.Hardware.MemoryFraction (fraction of total RAM)
//  3. agentFraction                      (--memory-fraction flag)
//
// Returns an error only if MemoryBudget is set but cannot be parsed.
func ResolveMemoryBudget(hardware *inferencev1alpha1.HardwareSpec, agentFraction float64) (ResolvedBudget, error) {
	if hardware != nil && hardware.MemoryBudget != "" {
		qty, err := resource.ParseQuantity(hardware.MemoryBudget)
		if err != nil {
			return ResolvedBudget{}, fmt.Errorf("invalid memoryBudget %q: %w", hardware.MemoryBudget, err)
		}
		if qty.Value() <= 0 {
			return ResolvedBudget{}, fmt.Errorf("memoryBudget must be positive, got %q", hardware.MemoryBudget)
		}
		return ResolvedBudget{
			Mode:   BudgetModeAbsolute,
			Bytes:  uint64(qty.Value()),
			Source: "crd-budget",
		}, nil
	}

	if hardware != nil && hardware.MemoryFraction != nil {
		f := *hardware.MemoryFraction
		if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1.0 {
			return ResolvedBudget{}, fmt.Errorf("memoryFraction must be between 0.0 (exclusive) and 1.0 (inclusive), got %f", f)
		}
		return ResolvedBudget{
			Mode:     BudgetModeFraction,
			Fraction: f,
			Source:   "crd-fraction",
		}, nil
	}

	return ResolvedBudget{
		Mode:     BudgetModeFraction,
		Fraction: agentFraction,
		Source:   "agent-flag",
	}, nil
}

// CheckMemoryBudgetAbsolute checks whether a model's estimated memory fits
// within a fixed byte budget (as opposed to a fraction of system memory).
func CheckMemoryBudgetAbsolute(budgetBytes uint64, estimate MemoryEstimate) *MemoryBudget {
	fits := estimate.TotalBytes <= budgetBytes

	var headroom uint64
	if fits {
		headroom = budgetBytes - estimate.TotalBytes
	}

	return &MemoryBudget{
		Fits:          fits,
		TotalBytes:    0, // not applicable for absolute budgets
		BudgetBytes:   budgetBytes,
		EstimateBytes: estimate.TotalBytes,
		HeadroomBytes: headroom,
	}
}

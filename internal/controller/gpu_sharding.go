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

package controller

import (
	"fmt"
	"strconv"
	"strings"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Multi-GPU sharding helpers. Translate the Model CRD's GPUShardingSpec into
// the llama.cpp --split-mode and --tensor-split flag values, including the
// layer-range → ratio math when a custom LayerSplit is provided.

// llama.cpp --split-mode values.
const (
	splitModeLayer = "layer"
	splitModeRow   = "row"
	splitModeNone  = "none"
)

// resolveSplitMode maps the Model CRD's sharding.Strategy enum to the llama.cpp
// --split-mode value. Unknown or missing values fall back to layer. The
// "pipeline" strategy is accepted for forward compatibility but falls back to
// layer since llama.cpp has no pipeline split-mode.
func resolveSplitMode(sharding *inferencev1alpha1.GPUShardingSpec) string {
	if sharding == nil {
		return splitModeLayer
	}
	switch sharding.Strategy {
	case splitModeRow, "tensor":
		return splitModeRow
	case splitModeNone:
		return splitModeNone
	case "pipeline":
		// llama.cpp has no pipeline split-mode; fall back to layer.
		return splitModeLayer
	case splitModeLayer, "":
		return splitModeLayer
	default:
		return splitModeLayer
	}
}

// calculateTensorSplit returns comma-separated ratios for llama.cpp --tensor-split flag.
// When sharding.LayerSplit is provided, layer ranges are converted to proportional ratios
// (e.g., ["0-24", "25-39"] becomes "5,3"). Falls back to equal split on any error.
func calculateTensorSplit(gpuCount int32, sharding *inferencev1alpha1.GPUShardingSpec) string {
	if gpuCount <= 1 {
		return ""
	}

	//nolint:gosec // G115: LayerSplit slice length is bounded by user-configured GPU count (≤8 per CRD)
	if sharding != nil && len(sharding.LayerSplit) > 0 && int32(len(sharding.LayerSplit)) == gpuCount {
		layerCounts := make([]int, len(sharding.LayerSplit))
		valid := true
		for i, split := range sharding.LayerSplit {
			start, end, err := parseLayerRange(split)
			if err != nil {
				valid = false
				break
			}
			layerCounts[i] = end - start + 1
		}
		if valid {
			g := layerCounts[0]
			for _, c := range layerCounts[1:] {
				g = gcd(g, c)
			}
			parts := make([]string, len(layerCounts))
			for i, c := range layerCounts {
				parts[i] = strconv.Itoa(c / g)
			}
			return strings.Join(parts, ",")
		}
	}

	ratios := make([]string, gpuCount)
	for i := range ratios {
		ratios[i] = "1"
	}
	return strings.Join(ratios, ",")
}

// parseLayerRange parses a "start-end" layer range string.
func parseLayerRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid layer range format: %q", s)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start layer in %q: %w", s, err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end layer in %q: %w", s, err)
	}
	if start < 0 || end < 0 || start > end {
		return 0, 0, fmt.Errorf("invalid layer range %q: start must be <= end and non-negative", s)
	}
	return start, end, nil
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

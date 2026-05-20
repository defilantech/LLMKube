//go:build !darwin

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
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// The non-darwin capability provider is a stub: it advertises whatever
// CapabilityOptions hands it (flag-supplied), with AvailableRAMGB ==
// StaticTotalRAMGB until M4 wires up live Linux memory probing.

func TestNewCapability_Other_PropagatesAllFlagSuppliedFields(t *testing.T) {
	p := NewCapability(CapabilityOptions{
		Accelerator:      foremanv1alpha1.FleetNodeAccelerator("cuda"),
		InstalledModels:  []string{"qwen3-coder-30b"},
		MaxContextTokens: 32768,
		TokensPerSecond:  85,
		StaticTotalRAMGB: 64,
	})
	cap := p.Capability()
	if string(cap.Accelerator) != "cuda" {
		t.Errorf("Accelerator = %q, want %q", cap.Accelerator, "cuda")
	}
	if cap.TotalRAMGB != 64 {
		t.Errorf("TotalRAMGB = %d, want 64", cap.TotalRAMGB)
	}
	if cap.AvailableRAMGB != 64 {
		t.Errorf("AvailableRAMGB = %d, want 64 (stub: equal to TotalRAMGB until M4)", cap.AvailableRAMGB)
	}
	if cap.MaxContextTokens != 32768 {
		t.Errorf("MaxContextTokens = %d, want 32768", cap.MaxContextTokens)
	}
	if cap.TokensPerSecond != 85 {
		t.Errorf("TokensPerSecond = %d, want 85", cap.TokensPerSecond)
	}
	if len(cap.InstalledModels) != 1 || cap.InstalledModels[0] != "qwen3-coder-30b" {
		t.Errorf("InstalledModels = %v", cap.InstalledModels)
	}
}

func TestNewCapability_Other_HonorsEmptyAccelerator(t *testing.T) {
	// On non-darwin, no default accelerator is filled in; v0.1 expects
	// the operator to set --accelerator explicitly. Confirm we don't
	// silently default to anything.
	p := NewCapability(CapabilityOptions{})
	cap := p.Capability()
	if string(cap.Accelerator) != "" {
		t.Errorf(
			"Accelerator = %q on non-darwin with empty options; want empty (operator must set explicitly)",
			cap.Accelerator,
		)
	}
}

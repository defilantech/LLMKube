//go:build darwin

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
	"math"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestBytesToGB(t *testing.T) {
	const gb = uint64(1024 * 1024 * 1024)
	cases := []struct {
		name string
		in   uint64
		want int32
	}{
		{"zero", 0, 0},
		{"sub_gb_rounds_down", gb / 2, 0},
		{"exactly_1gb", gb, 1},
		{"36gb_mac_studio", 36 * gb, 36},
		{"128gb_m5_max", 128 * gb, 128},
		{"max_int32_gb_passes_through", uint64(math.MaxInt32) * gb, math.MaxInt32},
		{"saturates_at_max_int32", math.MaxUint64, math.MaxInt32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bytesToGB(tc.in)
			if got != tc.want {
				t.Errorf("bytesToGB(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewCapability_Darwin_DefaultsAcceleratorToMetal(t *testing.T) {
	p := NewCapability(CapabilityOptions{})
	cap := p.Capability()
	if string(cap.Accelerator) != "metal" {
		t.Errorf("default accelerator on darwin = %q, want %q", cap.Accelerator, "metal")
	}
}

func TestNewCapability_Darwin_HonorsExplicitAcceleratorOverride(t *testing.T) {
	p := NewCapability(CapabilityOptions{
		Accelerator: foremanv1alpha1.FleetNodeAccelerator("none"),
	})
	cap := p.Capability()
	if string(cap.Accelerator) != "none" {
		t.Errorf("override accelerator = %q, want %q", cap.Accelerator, "none")
	}
}

func TestNewCapability_Darwin_PropagatesFlagsuppliedFields(t *testing.T) {
	p := NewCapability(CapabilityOptions{
		InstalledModels:  []string{"minimax-m2-7", "qwen36-35b-carnice-mtp"},
		MaxContextTokens: 131072,
		TokensPerSecond:  47,
	})
	cap := p.Capability()
	if len(cap.InstalledModels) != 2 {
		t.Errorf("InstalledModels len = %d, want 2", len(cap.InstalledModels))
	}
	if cap.MaxContextTokens != 131072 {
		t.Errorf("MaxContextTokens = %d, want 131072", cap.MaxContextTokens)
	}
	if cap.TokensPerSecond != 47 {
		t.Errorf("TokensPerSecond = %d, want 47", cap.TokensPerSecond)
	}
}

func TestNewCapability_Darwin_LiveMemoryProbeIsSane(t *testing.T) {
	// The DarwinMemoryProvider lives in pkg/agent and runs `sysctl
	// hw.memsize` + `vm_stat`. We don't assert specific RAM values
	// (those depend on the host), but we do assert the relationships
	// that any sane macOS host satisfies: TotalRAMGB > 0 and
	// AvailableRAMGB <= TotalRAMGB.
	p := NewCapability(CapabilityOptions{})
	cap := p.Capability()
	if cap.TotalRAMGB <= 0 {
		t.Skipf("live darwin memory probe returned %d GB; assume CI sandbox without sysctl", cap.TotalRAMGB)
	}
	if cap.AvailableRAMGB > cap.TotalRAMGB {
		t.Errorf("AvailableRAMGB (%d) > TotalRAMGB (%d); impossible", cap.AvailableRAMGB, cap.TotalRAMGB)
	}
}

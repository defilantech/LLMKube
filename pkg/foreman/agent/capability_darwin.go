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

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	llmkubeagent "github.com/defilantech/llmkube/pkg/agent"
)

// darwinCapability advertises the host as a Metal worker. Total / available
// RAM come from sysctl + vm_stat via the LLMKube metal-agent's existing
// DarwinMemoryProvider; the rest are flag-supplied for v0.1 (M5 will
// derive MaxContextTokens / TokensPerSecond from loaded model metadata).
type darwinCapability struct {
	mem              *llmkubeagent.DarwinMemoryProvider
	models           []string
	maxContext       int32
	tokensPerSec     int32
	acceleratorLabel foremanv1alpha1.FleetNodeAccelerator
}

// NewCapability is the cross-platform constructor. On darwin it returns a
// Metal capability provider backed by the existing metal-agent memory
// probes; on other platforms the build-tagged sibling returns a stub.
func NewCapability(opts CapabilityOptions) CapabilityProvider {
	acc := opts.Accelerator
	if acc == "" {
		acc = "metal"
	}
	return &darwinCapability{
		mem:              &llmkubeagent.DarwinMemoryProvider{},
		models:           opts.InstalledModels,
		maxContext:       opts.MaxContextTokens,
		tokensPerSec:     opts.TokensPerSecond,
		acceleratorLabel: acc,
	}
}

func (d *darwinCapability) Capability() foremanv1alpha1.FleetNodeCapability {
	totalB, _ := d.mem.TotalMemory()
	availB, _ := d.mem.AvailableMemory()
	return foremanv1alpha1.FleetNodeCapability{
		Accelerator:      d.acceleratorLabel,
		TotalRAMGB:       bytesToGB(totalB),
		AvailableRAMGB:   bytesToGB(availB),
		InstalledModels:  d.models,
		MaxContextTokens: d.maxContext,
		TokensPerSecond:  d.tokensPerSec,
	}
}

// bytesToGB safely narrows a byte count to int32 gigabytes. A 2.1 EB host
// would saturate the field; real machines clear int32 by orders of magnitude.
func bytesToGB(b uint64) int32 {
	gb := b / (1024 * 1024 * 1024)
	if gb > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(gb) //nolint:gosec // bounded above
}

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
	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// stubCapability is the v0.1 non-darwin fallback so the foreman-agent
// builds cross-platform. Dynamic memory probing on Linux / NVIDIA lands
// in M4 when ShadowStack joins the fleet; for now this advertises only
// the static fields the operator passes via flags.
type stubCapability struct {
	models           []string
	maxContext       int32
	tokensPerSec     int32
	totalRAMGB       int32
	acceleratorLabel foremanv1alpha1.FleetNodeAccelerator
}

// NewCapability returns the build-tagged provider.
func NewCapability(opts CapabilityOptions) CapabilityProvider {
	return &stubCapability{
		models:           opts.InstalledModels,
		maxContext:       opts.MaxContextTokens,
		tokensPerSec:     opts.TokensPerSecond,
		totalRAMGB:       opts.StaticTotalRAMGB,
		acceleratorLabel: opts.Accelerator,
	}
}

func (s *stubCapability) Capability() foremanv1alpha1.FleetNodeCapability {
	return foremanv1alpha1.FleetNodeCapability{
		Accelerator:      s.acceleratorLabel,
		TotalRAMGB:       s.totalRAMGB,
		AvailableRAMGB:   s.totalRAMGB,
		InstalledModels:  s.models,
		MaxContextTokens: s.maxContext,
		TokensPerSecond:  s.tokensPerSec,
	}
}

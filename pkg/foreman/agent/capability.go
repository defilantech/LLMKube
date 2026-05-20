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

// CapabilityOptions is the cross-platform constructor input for
// NewCapability. Fields the OS can probe (Total / Available RAM on
// darwin) are read at heartbeat time and not duplicated here.
type CapabilityOptions struct {
	// Accelerator labels the host's accelerator family. On darwin this
	// defaults to "metal"; on other platforms it must be set explicitly
	// in v0.1 (M4 will probe NVIDIA + similar).
	Accelerator foremanv1alpha1.FleetNodeAccelerator

	// InstalledModels are the Model CR names this node can load.
	InstalledModels []string

	// MaxContextTokens is the largest context window the loaded model
	// supports. Advertised verbatim; the scheduler uses it to filter
	// AgenticTasks whose RequiredCapability.MinContextTokens exceeds it.
	MaxContextTokens int32

	// TokensPerSecond is the operator's coarse decode-throughput estimate
	// for the loaded model. v0.1 takes this as a flag; v0.2 will
	// benchmark on heartbeat.
	TokensPerSecond int32

	// StaticTotalRAMGB is used only on platforms where memory probing
	// is not yet implemented (non-darwin in v0.1). On darwin it is
	// ignored in favor of the live sysctl value.
	StaticTotalRAMGB int32
}

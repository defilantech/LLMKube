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

// priorityWeights mirrors internal/controller/scheduling.go's priorityValues.
// Duplicated here because pkg/agent cannot import from internal/controller
// (Go internal-package rules). Keep these in sync; they encode the same
// semantic ordering used for both Kubernetes PriorityClass selection and,
// reused in this file, for memory-pressure eviction order under the metal
// agent.
//
// The enum values come from InferenceServiceSpec.Priority's CRD validation
// (critical, high, normal, low, batch). Lower numeric weight = lower priority
// = evicted first when the metal agent's MemoryWatchdog signals critical
// pressure. Reusing the same field for GPU scheduling and memory eviction
// is intentional: a workload the user marked "low" because it can wait for
// a GPU is also a workload that can be paused under memory pressure. If the
// two semantics diverge for some user, that is the moment to introduce an
// explicit EvictionPriority field on the spec.
var priorityWeights = map[string]int32{
	"critical": 1000000,
	"high":     100000,
	"normal":   10000,
	"low":      1000,
	"batch":    100,
}

// priorityWeight returns the numeric weight for the given priority enum value.
// Unknown or empty strings are treated as "normal" so a malformed CRD value
// does not accidentally promote a workload above explicitly-set ones.
func priorityWeight(p string) int32 {
	if w, ok := priorityWeights[p]; ok {
		return w
	}
	return priorityWeights["normal"]
}

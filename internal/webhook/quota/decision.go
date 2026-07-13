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

// Package quota holds the pure admission-decision logic for GPUQuota.
// It is intentionally free of Kubernetes or webhook dependencies so the
// allow/deny rules can be exhaustively unit-tested in isolation. The
// controller-runtime webhook that calls this lives in internal/controller
// and is wired up separately.
package quota

import (
	"fmt"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// priorityWeights mirrors pkg/agent/priority.go and internal/controller/
// scheduling.go. The three copies encode the same semantic ordering used
// for both Kubernetes PriorityClass selection and GPU admission. Keep them
// in sync; if they ever diverge the ordering is no longer well-defined.
var priorityWeights = map[string]int32{
	"critical": 1000000,
	"high":     100000,
	"normal":   10000,
	"low":      1000,
	"batch":    100,
}

// priorityWeight returns the numeric weight for the given priority enum
// value. Unknown or empty strings are treated as "normal" so a malformed
// CRD value does not accidentally promote a workload above explicitly-set
// ones.
func priorityWeight(p string) int32 {
	if w, ok := priorityWeights[p]; ok {
		return w
	}
	return priorityWeights["normal"]
}

// Usage is the current state of a quota: how many GPUs and how much VRAM
// are already allocated.
type Usage struct {
	// GPUCount is the number of GPUs currently allocated under the quota.
	GPUCount int32
	// VRAMBytes is the total VRAM (bytes) currently allocated under the
	// quota. Zero means no VRAM has been allocated yet.
	VRAMBytes int64
}

// Incoming describes a single admission request against the quota.
type Incoming struct {
	// GPUCount is the number of GPUs the requestor wants to allocate.
	GPUCount int32
	// VRAMBytes is the VRAM (bytes) the requestor wants to allocate.
	VRAMBytes int64
	// Priority is the requestor's declared priority (critical, high,
	// normal, low, batch).
	Priority string
}

// Decide evaluates a single admission request against a GPUQuota and
// returns whether to allow it plus a human-readable reason. The function
// is pure: it does not read live state, talk to an API server, or consult
// any external cost budget. The caller is responsible for passing in the
// current usage and the cost-budget status.
//
// Rules (in order):
//
//  1. If CostBudgetRef is set on the spec and costBudgetBreached is true,
//     deny with a cost-budget reason.
//  2. If current+incoming GPUCount would exceed spec.GPUCount, deny with
//     an over-GPU-count reason.
//  3. If spec.VRAMBytes is non-zero and current+incoming VRAMBytes would
//     exceed it, deny with an over-VRAM reason.
//  4. If incoming.Priority is below spec.MinPriority, deny with a
//     priority reason.
//
// If none of the above apply, allow.
func Decide(spec inferencev1alpha1.GPUQuotaSpec, current Usage, incoming Incoming, costBudgetBreached bool) (allow bool, reason string) {
	// Cost budget check. The cost budget is the highest-priority gate:
	// if the dollar budget is exhausted, nothing gets through regardless
	// of priority or headroom.
	if spec.CostBudgetRef != "" && costBudgetBreached {
		return false, fmt.Sprintf("cost budget %q exhausted", spec.CostBudgetRef)
	}

	// GPU count check. GPUCount is a required field and the hard cap on
	// GPUs the quota allows; unlike VRAMBytes, zero means "no GPUs allowed"
	// (not "unlimited"), so the comparison always runs.
	if current.GPUCount+incoming.GPUCount > spec.GPUCount {
		return false, fmt.Sprintf("would exceed gpuCount %d (current %d + requested %d)",
			spec.GPUCount, current.GPUCount, incoming.GPUCount)
	}

	// VRAM check. VRAMBytes==0 on the spec means "no cap", so skip the
	// check in that case.
	if spec.VRAMBytes > 0 {
		if current.VRAMBytes+incoming.VRAMBytes > spec.VRAMBytes {
			return false, fmt.Sprintf("would exceed vramBytes %d (current %d + requested %d)",
				spec.VRAMBytes, current.VRAMBytes, incoming.VRAMBytes)
		}
	}

	// Priority check. An empty minPriority on the spec means "no minimum",
	// so any incoming priority is fine. An empty incoming priority is
	// treated as "normal" (the default weight) so a malformed request
	// does not accidentally pass a high bar.
	if spec.MinPriority != "" {
		if priorityWeight(incoming.Priority) < priorityWeight(spec.MinPriority) {
			return false, fmt.Sprintf("priority %q below minimum %q", incoming.Priority, spec.MinPriority)
		}
	}

	return true, "allowed"
}

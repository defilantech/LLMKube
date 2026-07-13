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

package quota

import (
	"testing"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// want is the expected allow value for a Decide call.
type want struct {
	allow bool
}

func TestDecide_InQuotaAllow(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:    8,
		VRAMBytes:   64 << 30, // 64 GiB
		MinPriority: "low",
	}
	current := Usage{GPUCount: 2, VRAMBytes: 16 << 30}
	incoming := Incoming{GPUCount: 2, VRAMBytes: 16 << 30, Priority: "high"}

	allow, reason := Decide(spec, current, incoming, false)
	if !allow {
		t.Fatalf("expected allow, got deny: %s", reason)
	}
}

func TestDecide_OverGPUCount(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:  4,
		VRAMBytes: 32 << 30,
	}
	current := Usage{GPUCount: 3, VRAMBytes: 8 << 30}
	incoming := Incoming{GPUCount: 2, VRAMBytes: 4 << 30, Priority: "normal"}

	allow, reason := Decide(spec, current, incoming, false)
	if allow {
		t.Fatalf("expected deny, got allow")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestDecide_OverVRAMBytes(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:  8,
		VRAMBytes: 32 << 30, // 32 GiB
	}
	current := Usage{GPUCount: 2, VRAMBytes: 28 << 30}
	incoming := Incoming{GPUCount: 1, VRAMBytes: 8 << 30, Priority: "normal"}

	allow, reason := Decide(spec, current, incoming, false)
	if allow {
		t.Fatalf("expected deny, got allow: %s", reason)
	}
}

func TestDecide_MinPrioritySatisfied(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:    4,
		MinPriority: "high",
	}
	current := Usage{GPUCount: 0}
	incoming := Incoming{GPUCount: 1, Priority: "critical"}

	allow, reason := Decide(spec, current, incoming, false)
	if !allow {
		t.Fatalf("expected allow, got deny: %s", reason)
	}
}

func TestDecide_MinPriorityViolated(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:    4,
		MinPriority: "high",
	}
	current := Usage{GPUCount: 0}
	incoming := Incoming{GPUCount: 1, Priority: "low"}

	allow, _ := Decide(spec, current, incoming, false)
	if allow {
		t.Fatalf("expected deny, got allow")
	}
}

func TestDecide_MinPriorityAtEachLevel(t *testing.T) {
	// Verify the priority ordering critical > high > normal > low > batch
	// by checking that each level satisfies its own minimum but not the
	// next-higher minimum.
	// priorities[0] = critical (highest), priorities[4] = batch (lowest)
	priorities := []string{"critical", "high", "normal", "low", "batch"}
	for i, minP := range priorities {
		t.Run(minP, func(t *testing.T) {
			spec := inferencev1alpha1.GPUQuotaSpec{
				GPUCount:    4,
				MinPriority: minP,
			}
			current := Usage{GPUCount: 0}

			// Incoming at or above minP should be allowed.
			// Since priorities are in descending order, indices 0..i are at or above minP.
			for j := 0; j <= i; j++ {
				incP := priorities[j]
				incoming := Incoming{GPUCount: 1, Priority: incP}
				allow, reason := Decide(spec, current, incoming, false)
				if !allow {
					t.Errorf("priority %q with min %q: expected allow, got deny: %s", incP, minP, reason)
				}
			}

			// Incoming below minP should be denied.
			// Indices i+1..len-1 are below minP.
			for j := i + 1; j < len(priorities); j++ {
				incP := priorities[j]
				incoming := Incoming{GPUCount: 1, Priority: incP}
				allow, _ := Decide(spec, current, incoming, false)
				if allow {
					t.Errorf("priority %q with min %q: expected deny, got allow", incP, minP)
				}
			}
		})
	}
}

func TestDecide_CostBudgetBreached(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:      8,
		CostBudgetRef: "budget-prod",
	}
	current := Usage{GPUCount: 0}
	incoming := Incoming{GPUCount: 1, Priority: "critical"}

	allow, reason := Decide(spec, current, incoming, true)
	if allow {
		t.Fatalf("expected deny when cost budget breached, got allow")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestDecide_CostBudgetNotBreached(t *testing.T) {
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount:      8,
		CostBudgetRef: "budget-prod",
	}
	current := Usage{GPUCount: 0}
	incoming := Incoming{GPUCount: 1, Priority: "critical"}

	allow, reason := Decide(spec, current, incoming, false)
	if !allow {
		t.Fatalf("expected allow when cost budget not breached, got deny: %s", reason)
	}
}

func TestDecide_CostBudgetRefEmpty(t *testing.T) {
	// When CostBudgetRef is empty, costBudgetBreached should be ignored.
	spec := inferencev1alpha1.GPUQuotaSpec{
		GPUCount: 8,
	}
	current := Usage{GPUCount: 0}
	incoming := Incoming{GPUCount: 1, Priority: "critical"}

	allow, reason := Decide(spec, current, incoming, true)
	if !allow {
		t.Fatalf("expected allow when CostBudgetRef is empty, got deny: %s", reason)
	}
}

func TestDecide_Combinations(t *testing.T) {
	tests := []struct {
		name     string
		spec     inferencev1alpha1.GPUQuotaSpec
		current  Usage
		incoming Incoming
		breached bool
		want     want
	}{
		{
			name: "allow: in quota, priority ok, no cost budget",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:    4,
				MinPriority: "normal",
			},
			current:  Usage{GPUCount: 1},
			incoming: Incoming{GPUCount: 1, Priority: "high"},
			want:     want{allow: true},
		},
		{
			name: "deny: over gpuCount AND below priority",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:    2,
				MinPriority: "high",
			},
			current:  Usage{GPUCount: 2},
			incoming: Incoming{GPUCount: 1, Priority: "low"},
			want:     want{allow: false},
		},
		{
			name: "deny: cost budget breached overrides allow",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:      8,
				CostBudgetRef: "budget-prod",
			},
			current:  Usage{GPUCount: 0},
			incoming: Incoming{GPUCount: 1, Priority: "critical"},
			breached: true,
			want:     want{allow: false},
		},
		{
			name: "allow: cost budget breached but no ref set",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount: 8,
			},
			current:  Usage{GPUCount: 0},
			incoming: Incoming{GPUCount: 1, Priority: "critical"},
			breached: true,
			want:     want{allow: true},
		},
		{
			name: "deny: over vram AND over gpuCount (gpuCount checked first)",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:  2,
				VRAMBytes: 16 << 30,
			},
			current:  Usage{GPUCount: 2, VRAMBytes: 12 << 30},
			incoming: Incoming{GPUCount: 1, VRAMBytes: 8 << 30, Priority: "normal"},
			want:     want{allow: false},
		},
		{
			name: "allow: exactly at quota limit",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:    4,
				VRAMBytes:   32 << 30,
				MinPriority: "batch",
			},
			current:  Usage{GPUCount: 2, VRAMBytes: 16 << 30},
			incoming: Incoming{GPUCount: 2, VRAMBytes: 16 << 30, Priority: "batch"},
			want:     want{allow: true},
		},
		{
			name: "deny: exactly at quota limit but priority too low",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount:    4,
				MinPriority: "high",
			},
			current:  Usage{GPUCount: 2},
			incoming: Incoming{GPUCount: 2, Priority: "batch"},
			want:     want{allow: false},
		},
		{
			name: "deny: gpuCount 0 cap rejects any positive request",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount: 0,
			},
			current:  Usage{GPUCount: 0},
			incoming: Incoming{GPUCount: 1},
			want:     want{allow: false},
		},
		{
			name: "allow: gpuCount 0 cap accepts a zero-gpu request",
			spec: inferencev1alpha1.GPUQuotaSpec{
				GPUCount: 0,
			},
			current:  Usage{GPUCount: 0},
			incoming: Incoming{GPUCount: 0},
			want:     want{allow: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allow, reason := Decide(tt.spec, tt.current, tt.incoming, tt.breached)
			if allow != tt.want.allow {
				t.Errorf("allow = %v, want %v (reason: %s)", allow, tt.want.allow, reason)
			}
		})
	}
}

func TestPriorityWeight(t *testing.T) {
	tests := []struct {
		priority string
		want     int32
	}{
		{"critical", 1000000},
		{"high", 100000},
		{"normal", 10000},
		{"low", 1000},
		{"batch", 100},
		{"", 10000},        // unknown defaults to normal
		{"unknown", 10000}, // unknown defaults to normal
	}

	for _, tt := range tests {
		t.Run(tt.priority, func(t *testing.T) {
			got := priorityWeight(tt.priority)
			if got != tt.want {
				t.Errorf("priorityWeight(%q) = %d, want %d", tt.priority, got, tt.want)
			}
		})
	}
}

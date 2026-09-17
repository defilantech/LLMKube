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

package v1alpha1

import (
	"reflect"
	"testing"
)

func TestScanGateResolve(t *testing.T) {
	// A fresh pointer per IgnoreUnfixed case, so the shared bool is never
	// aliased across table rows.
	ignoreTrue := true
	ignoreFalse := false

	tests := []struct {
		name string
		gate *ScanGate
		want ResolvedScan
	}{
		{
			name: "nil gate resolves to full defaults",
			gate: nil,
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "empty gate resolves to full defaults",
			gate: &ScanGate{},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "empty images is nil-means-all",
			gate: &ScanGate{},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "explicit images override nil-means-all",
			gate: &ScanGate{
				Images: []string{"controller", "foreman-agent"},
			},
			want: ResolvedScan{
				Images:        []string{"controller", "foreman-agent"},
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "severity override",
			gate: &ScanGate{
				Severity: []string{"CRITICAL"},
			},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "ignoreUnfixed explicit true",
			gate: &ScanGate{
				IgnoreUnfixed: &ignoreTrue,
			},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "ignoreUnfixed explicit false",
			gate: &ScanGate{
				IgnoreUnfixed: &ignoreFalse,
			},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: false,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "builder image override",
			gate: &ScanGate{
				BuilderImage: "golang:1.25",
			},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.25",
				RunnerImage:   "alpine:3.24",
			},
		},
		{
			name: "runner image override",
			gate: &ScanGate{
				RunnerImage: "alpine:3.23",
			},
			want: ResolvedScan{
				Images:        nil,
				Severity:      []string{"CRITICAL", "HIGH"},
				IgnoreUnfixed: true,
				BuilderImage:  "golang:1.26",
				RunnerImage:   "alpine:3.23",
			},
		},
		{
			name: "all overrides",
			gate: &ScanGate{
				Images:        []string{"router-proxy"},
				Severity:      []string{"CRITICAL", "HIGH", "MEDIUM"},
				IgnoreUnfixed: &ignoreFalse,
				BuilderImage:  "golang:1.25",
				RunnerImage:   "alpine:3.23",
			},
			want: ResolvedScan{
				Images:        []string{"router-proxy"},
				Severity:      []string{"CRITICAL", "HIGH", "MEDIUM"},
				IgnoreUnfixed: false,
				BuilderImage:  "golang:1.25",
				RunnerImage:   "alpine:3.23",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.gate.Resolve()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScanGateIsZero(t *testing.T) {
	ignoreTrue := true

	tests := []struct {
		name string
		gate *ScanGate
		want bool
	}{
		{name: "nil gate is zero", gate: nil, want: true},
		{name: "empty gate is zero", gate: &ScanGate{}, want: true},
		{name: "images set is not zero", gate: &ScanGate{Images: []string{"controller"}}, want: false},
		{name: "severity set is not zero", gate: &ScanGate{Severity: []string{"CRITICAL"}}, want: false},
		// Any non-nil IgnoreUnfixed pointer, even pointing at false, is a
		// meaningful declaration.
		{name: "ignoreUnfixed true is not zero", gate: &ScanGate{IgnoreUnfixed: &ignoreTrue}, want: false},
		{name: "ignoreUnfixed false pointer is not zero", gate: &ScanGate{IgnoreUnfixed: newBool(false)}, want: false},
		{name: "builder image set is not zero", gate: &ScanGate{BuilderImage: "golang:1.25"}, want: false},
		{name: "runner image set is not zero", gate: &ScanGate{RunnerImage: "alpine:3.23"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.gate.IsZero()
			if got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// newBool returns a pointer to b. Kept local to the test so the table rows
// that each need a distinct *bool do not share one address.
func newBool(b bool) *bool { return &b }

func TestScanGateResolveIsolatesDefaultSeverity(t *testing.T) {
	// Resolve must hand out a fresh copy of the shared default severity
	// slice: mutating one result must not corrupt the default seen by the
	// next call.
	first := (*ScanGate)(nil).Resolve()
	first.Severity = append(first.Severity, "MEDIUM")

	second := (*ScanGate)(nil).Resolve()
	want := []string{"CRITICAL", "HIGH"}
	if !reflect.DeepEqual(second.Severity, want) {
		t.Errorf("default severity was mutated across calls: got %v, want %v",
			second.Severity, want)
	}
}

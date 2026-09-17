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

// Default scan images, byte-aligned with hack/scan-images.sh so the local
// reproduce matches the CI gate it stands in for.
const (
	// DefaultScanBuilderImage is the image that builds each release target
	// binary (CGO_ENABLED=0, linux/amd64) before the image is assembled.
	DefaultScanBuilderImage = "golang:1.26"
	// DefaultScanRunnerImage is the image that assembles each release image
	// (daemonless buildah) and runs the Trivy scan over it.
	DefaultScanRunnerImage = "alpine:3.24"
)

// defaultScanSeverity is the Trivy severity floor the CI gate scans at
// (hack/scan-images.sh: --severity CRITICAL,HIGH). It is a package-level
// shared slice: Resolve must hand out a fresh copy of it on every call and
// never return the shared slice, so a caller mutating its result cannot
// corrupt the default.
var defaultScanSeverity = []string{"CRITICAL", "HIGH"}

// ScanGate declares a reproducible container-image scan gate for a task:
// which built-in release-image targets to scan, the Trivy severity floor, and
// whether unfixed findings are ignored. Consumed by the scan-gate executor;
// unset means "no scan gate" (unchanged task behavior). Mirrors GateProfile's
// preset+override design.
type ScanGate struct {
	// Images lists the built-in scan-target ids to scan (e.g. "controller",
	// "foreman-agent"). Empty means all built-in targets: a nil/empty
	// Images is the nil-means-all rule, and Resolve passes it through as a
	// nil ResolvedScan.Images. Unknown ids are rejected by Resolve's
	// caller; Resolve itself ignores the list beyond the empty-means-all
	// rule and leaves per-id validation to the tool.
	// +optional
	Images []string `json:"images,omitempty"`

	// Severity is the Trivy severity floor. Empty defaults to
	// {"CRITICAL","HIGH"} (CI's floor).
	// +optional
	Severity []string `json:"severity,omitempty"`

	// IgnoreUnfixed mirrors Trivy's --ignore-unfixed. nil defaults to true
	// (CI's setting: only findings with an available fix block).
	// +optional
	IgnoreUnfixed *bool `json:"ignoreUnfixed,omitempty"`

	// BuilderImage runs the Go build of each target binary. Empty defaults
	// to the built-in builder image ("golang:1.26").
	// +optional
	BuilderImage string `json:"builderImage,omitempty"`

	// RunnerImage assembles and scans each image (daemonless buildah +
	// pinned trivy). Empty defaults to the built-in runner ("alpine:3.24").
	// +optional
	RunnerImage string `json:"runnerImage,omitempty"`
}

// ResolvedScan is the concrete scan configuration after merging a
// ScanGate with its defaults. All fields populated.
type ResolvedScan struct {
	// Images lists the built-in scan-target ids to scan. Nil means "all
	// built-in targets" (the nil-means-all rule, matching an empty
	// ScanGate.Images); a non-nil slice is the exact set to scan.
	Images []string

	// Severity is the Trivy severity floor to apply.
	Severity []string

	// IgnoreUnfixed mirrors Trivy's --ignore-unfixed.
	IgnoreUnfixed bool

	// BuilderImage is the image that builds each target binary.
	BuilderImage string

	// RunnerImage is the image that assembles and scans each release image.
	RunnerImage string
}

// Resolve merges a ScanGate with its defaults and returns the concrete
// ResolvedScan. A nil receiver or an empty field resolves to the built-in
// default. Only non-empty fields from the gate override the default; empty
// fields keep the default value.
//
// Resolve is a pure function with no I/O. It never returns the shared
// default severity slice: a fresh copy is made on every call, so a caller
// mutating the result cannot corrupt the default.
func (g *ScanGate) Resolve() ResolvedScan {
	resolved := ResolvedScan{
		// Images stays nil: the nil-means-all rule. An explicit non-empty
		// list is copied into the result below.
		Images:        nil,
		Severity:      append([]string(nil), defaultScanSeverity...),
		IgnoreUnfixed: true,
		BuilderImage:  DefaultScanBuilderImage,
		RunnerImage:   DefaultScanRunnerImage,
	}

	if g == nil {
		return resolved
	}

	// Overlay explicit overrides from the gate.
	if len(g.Images) > 0 {
		resolved.Images = append([]string(nil), g.Images...)
	}
	if len(g.Severity) > 0 {
		resolved.Severity = append([]string(nil), g.Severity...)
	}
	if g.IgnoreUnfixed != nil {
		resolved.IgnoreUnfixed = *g.IgnoreUnfixed
	}
	if g.BuilderImage != "" {
		resolved.BuilderImage = g.BuilderImage
	}
	if g.RunnerImage != "" {
		resolved.RunnerImage = g.RunnerImage
	}

	return resolved
}

// IsZero reports whether no meaningful scan configuration is declared: a nil
// receiver, or a gate whose every field is zero/empty/nil. The executor uses
// it to decide "no scan gate" (unchanged task behavior).
func (g *ScanGate) IsZero() bool {
	if g == nil {
		return true
	}
	return len(g.Images) == 0 &&
		len(g.Severity) == 0 &&
		g.IgnoreUnfixed == nil &&
		g.BuilderImage == "" &&
		g.RunnerImage == ""
}

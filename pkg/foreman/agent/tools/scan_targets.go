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

package tools

import (
	"fmt"
	"strings"
)

// ScanTarget is one built-in release image the scan gate can reproduce:
// the GoReleaser-built binary staged under the $TARGETPLATFORM layout the
// Dockerfile COPYs from (exactly what hack/scan-images.sh stages, so the
// assembled image is representative of what GoReleaser will push).
type ScanTarget struct {
	// ID is the scan-target identifier, e.g. "controller".
	ID string
	// Image is the release image reference, e.g.
	// "ghcr.io/defilantech/llmkube-controller".
	Image string
	// Dockerfile is the GoReleaser Dockerfile at the repo root, e.g.
	// "Dockerfile.goreleaser".
	Dockerfile string
	// Binary is the built binary name the Dockerfile COPYs, e.g. "manager".
	Binary string
	// Main is the go-build argument (package or main-file path), e.g.
	// "./cmd/main.go".
	Main string
}

// builtInScanTargets mirrors the ALL table in hack/scan-images.sh verbatim,
// in script order. It is the clean-room reproduction's target set: what the
// CI release gate scans is what the scan Job scans, or the gate verdict
// means something different from the CI gate it stands in for.
// scan_targets_parity_test.go fails when either side drifts.
var builtInScanTargets = []ScanTarget{
	{
		ID:         "controller",
		Image:      "ghcr.io/defilantech/llmkube-controller",
		Dockerfile: "Dockerfile.goreleaser",
		Binary:     "manager",
		Main:       "./cmd/main.go",
	},
	{
		ID:         "foreman-operator",
		Image:      "ghcr.io/defilantech/llmkube-foreman-operator",
		Dockerfile: "Dockerfile.foreman-operator.goreleaser",
		Binary:     "foreman-operator",
		Main:       "./cmd/foreman-operator",
	},
	{
		ID:         "foreman-agent",
		Image:      "ghcr.io/defilantech/llmkube-foreman-agent",
		Dockerfile: "Dockerfile.foreman-agent.goreleaser",
		Binary:     "foreman-agent",
		Main:       "./cmd/foreman-agent",
	},
	{
		ID:         "router-proxy",
		Image:      "ghcr.io/defilantech/llmkube-router-proxy",
		Dockerfile: "Dockerfile.router-proxy.goreleaser",
		Binary:     "router-proxy",
		Main:       "./cmd/router-proxy",
	},
}

// DefaultScanSeverity is the Trivy severity floor the CI gate scans at
// (hack/scan-images.sh: --severity CRITICAL,HIGH). Kept byte-aligned with
// the script by scan_targets_parity_test.go; the api ScanGate default
// (api/foreman/v1alpha1) is the CRD-facing twin of this value.
var DefaultScanSeverity = []string{"CRITICAL", "HIGH"}

// DefaultScanIgnoreUnfixed mirrors Trivy's --ignore-unfixed, CI's setting:
// only findings with an available fix block the gate.
const DefaultScanIgnoreUnfixed = true

// DefaultScanTargetIDs returns a copy of the built-in target ids in script
// order. A copy, so a caller sorting or appending cannot reorder the
// built-in table for everyone else.
func DefaultScanTargetIDs() []string {
	out := make([]string, 0, len(builtInScanTargets))
	for _, t := range builtInScanTargets {
		out = append(out, t.ID)
	}
	return out
}

// resolveScanTargets maps an optional image-id list onto the built-in
// target table. nil/empty means all targets (the nil-means-all rule); a
// non-empty list selects the named targets, returned in script order (and
// deduplicated — a target appears in a scan at most once). An unknown id
// is an error naming the first one, checked case-sensitively in argument
// order so the message points at exactly what the caller got wrong.
func resolveScanTargets(images []string) ([]ScanTarget, error) {
	if len(images) == 0 {
		out := make([]ScanTarget, len(builtInScanTargets))
		copy(out, builtInScanTargets)
		return out, nil
	}

	known := make(map[string]bool, len(builtInScanTargets))
	for _, t := range builtInScanTargets {
		known[t.ID] = true
	}
	for _, id := range images {
		if !known[id] {
			return nil, fmt.Errorf("unknown scan image %q; known targets: %s",
				id, strings.Join(DefaultScanTargetIDs(), ", "))
		}
	}

	want := make(map[string]bool, len(images))
	for _, id := range images {
		want[id] = true
	}
	out := make([]ScanTarget, 0, len(want))
	for _, t := range builtInScanTargets {
		if want[t.ID] {
			out = append(out, t)
		}
	}
	return out, nil
}

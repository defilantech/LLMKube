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
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/grounding"
)

// checkGroundingBreadth is the advisory-tier companion to checkReferenceGrounding.
// It loads the broadened ground truth (chart resource names + exporter metric
// prefixes) and flags doc references that look like external metric names or
// chart resource names but are not grounded in either set. Severity is "minor"
// (non-blocking); the finding is surfaced to the reviewer via the advisory
// output rather than fed back to the coder loop.
//
// Fail-open: any load or diff error returns (false, "") so the advisory net
// never blocks a coder on its own failure.
func checkGroundingBreadth(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	gt, err := grounding.LoadGroundTruth(
		filepath.Join(workspace, "config/crd/bases"),
		workspace,
		"",
	)
	if err != nil {
		return false, ""
	}

	added, err := grounding.AddedLines(
		ctx, workspace, grounding.CommandRunner(run), "HEAD", []string{"*.md", "*.yaml", "*.yml"},
	)
	if err != nil {
		return false, ""
	}

	findings := grounding.DetectUngroundedReferences(added, gt)

	// Filter to only the "minor" severity findings produced by the advisory
	// exporter-metric check. The "blocker" findings are already handled by
	// checkReferenceGrounding in the block tier.
	var advisoryFindings []grounding.Finding
	for _, f := range findings {
		if f.Severity == "minor" {
			advisoryFindings = append(advisoryFindings, f)
		}
	}
	if len(advisoryFindings) == 0 {
		return false, ""
	}

	var b strings.Builder
	b.WriteString("Advisory: doc references that may be hallucinated (verify each):\n")
	for _, f := range advisoryFindings {
		fmt.Fprintf(&b, "  - %s\n", f.String())
	}
	return true, b.String()
}

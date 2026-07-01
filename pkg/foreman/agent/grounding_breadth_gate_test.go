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
	"errors"
	"strings"
	"testing"
)

// TestCheckGroundingBreadth_FlagsHallucinatedMetric verifies that
// checkGroundingBreadth returns failed=true with output naming the ungrounded
// token when a changed doc cites a hallucinated exporter metric name.
func TestCheckGroundingBreadth_FlagsHallucinatedMetric(t *testing.T) {
	// The fake runner returns a staged diff that adds a line referencing the
	// hallucinated metric dcgm_gpu_utilization. AddedLines calls:
	//   git add -A -- *.md *.yaml *.yml   (intent-to-add / stage; best-effort)
	//   git diff --cached --unified=0 --src-prefix=a/ --dst-prefix=b/ HEAD -- *.md *.yaml *.yml
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) > 0 && args[0] == "add":
			return "", nil // staging: best-effort
		case len(args) > 0 && args[0] == "diff":
			return "+++ b/docs/observability.md\n" +
				"@@ -0,0 +1,1 @@\n" +
				"+scrape dcgm_gpu_utilization from the DCGM exporter\n", nil
		}
		return "", nil
	}

	// LoadGroundTruth expects a valid crdBasesDir; use a non-existent path so
	// loadCRDBases returns an error... wait, it returns nil for empty string
	// but not for missing dirs. Use empty string for crdBasesDir so the CRD
	// scan is skipped. The metricsDir also needs to be empty or a real dir so
	// chart scanning is skipped. We rely on the static ExporterMetricPrefixes
	// seeded by LoadGroundTruth.
	//
	// checkGroundingBreadth uses workspace as metricsDir for scanMetrics and
	// filepath.Join(workspace,"charts") for scanChartResources. With a
	// non-existent workspace the scans are no-ops (fail-open); the static
	// ExporterMetricPrefixes are still seeded.
	workspace := t.TempDir() // empty dir: no CRDs, no Go files, no charts

	failed, output := checkGroundingBreadth(context.Background(), workspace, run)
	if !failed {
		t.Fatal("expected checkGroundingBreadth to return failed=true for dcgm_gpu_utilization")
	}
	if !strings.Contains(output, "dcgm_gpu_utilization") {
		t.Errorf("output should name the ungrounded token; got: %q", output)
	}
	if !strings.Contains(output, "Advisory") {
		t.Errorf("output should be prefixed with 'Advisory'; got: %q", output)
	}
}

// TestCheckGroundingBreadth_FailOpenOnDiffError verifies that a diff error
// does not cause the advisory check to block (fail-open posture).
func TestCheckGroundingBreadth_FailOpenOnDiffError(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			return "", errors.New("git diff failed")
		}
		return "", nil
	}

	workspace := t.TempDir()
	failed, output := checkGroundingBreadth(context.Background(), workspace, run)
	if failed {
		t.Errorf("should fail-open on diff error, but got failed=true output=%q", output)
	}
}

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
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The parity guard for the scan gate, in the spirit of the AGENTS.md
// runtime-arg parity tests: hack/scan-images.sh IS the release gate, and
// the scan Job is its clean-room reproduction. A GATE consumer reads a
// SCAN-PASS as "the CI image-scan gate would pass", and that reading is
// only true while the tool's target table and scan flags are byte-equal
// to the script's. These tests parse the script and compare; a future
// edit to either side that diverges fails here with a message naming
// both files.
//
// The path is relative to this package's dir: pkg/foreman/agent/tools ->
// repo root is four levels up (the gate CI-sync test uses the same depth
// for .github/workflows).
const scanScriptPath = "../../../../hack/scan-images.sh"

func readScanScript(t *testing.T) string {
	t.Helper()
	buf, err := os.ReadFile(scanScriptPath)
	if err != nil {
		t.Fatalf("read scan script %s: %v", scanScriptPath, err)
	}
	return string(buf)
}

// scanScriptAllRows returns the quoted rows of the script's ALL=( ... )
// target table, in script order, joined back with "|". It fails loudly if
// the block cannot be found: a parity test that silently parses nothing
// tests nothing (the #1072 lesson from the gate's non-go lint tests).
func scanScriptAllRows(t *testing.T, src string) []string {
	t.Helper()
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "ALL=(" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("ALL=( table not found in %s; the parity guard parsed nothing", scanScriptPath)
	}
	var rows []string
	for _, l := range lines[start+1:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == ")" {
			if len(rows) == 0 {
				t.Fatalf("ALL=( table in %s is empty; the parity guard parsed nothing", scanScriptPath)
			}
			return rows
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, `"`) || !strings.HasSuffix(trimmed, `"`) {
			t.Fatalf("unrecognised ALL= row in %s: %q (expected a quoted pipe-joined row)", scanScriptPath, l)
		}
		rows = append(rows, strings.Trim(trimmed, `"`))
	}
	t.Fatalf("unterminated ALL=( table in %s", scanScriptPath)
	return nil
}

// TestBuiltInScanTargetsMatchScanScript is the target-table drift guard:
// builtInScanTargets must equal the script's ALL rows field-for-field
// (id|image|dockerfile|binary|main) in the same order, and
// DefaultScanTargetIDs must equal the script's default want list.
func TestBuiltInScanTargetsMatchScanScript(t *testing.T) {
	src := readScanScript(t)
	rows := scanScriptAllRows(t, src)

	if len(rows) != len(builtInScanTargets) {
		t.Fatalf("scan-target count diverged: %s has %d ALL rows, but "+
			"pkg/foreman/agent/tools/scan_targets.go builtInScanTargets has %d; "+
			"update both files together", scanScriptPath, len(rows), len(builtInScanTargets))
	}

	for i, row := range rows {
		fields := strings.Split(row, "|")
		if len(fields) != 5 {
			t.Fatalf("ALL row %d in %s is not id|image|dockerfile|binary|main: %q", i, scanScriptPath, row)
		}
		got := builtInScanTargets[i]
		want := struct{ id, image, dockerfile, binary, main string }{fields[0], fields[1], fields[2], fields[3], fields[4]}
		if got.ID != want.id || got.Image != want.image || got.Dockerfile != want.dockerfile ||
			got.Binary != want.binary || got.Main != want.main {
			t.Errorf("scan target %d diverged: %s row is %q but builtInScanTargets[%d] is %+v; "+
				"hack/scan-images.sh and pkg/foreman/agent/tools/scan_targets.go must change together",
				i, scanScriptPath, row, i, got)
		}
	}

	wantRe := regexp.MustCompile(`want="\$\{IMAGES:-(.+)\}"`)
	m := wantRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("default want list (want=\"${IMAGES:-...}\") not found in %s; "+
			"the parity guard parsed nothing", scanScriptPath)
	}
	wantIDs := strings.Fields(m[1])
	if got := DefaultScanTargetIDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("DefaultScanTargetIDs() = %v, script default want list = %v; "+
			"update scan_targets.go (or the script) so the order matches", got, wantIDs)
	}
}

// TestScanGateDefaultsMatchScanScript pins the scan-flags half of the
// parity: the severity floor, --ignore-unfixed, --exit-code 1 and
// --format table the script passes to trivy must be exactly what the
// scan Job template renders at its defaults, and the builder must stage
// binaries with the script's GoReleaser build flags.
func TestScanGateDefaultsMatchScanScript(t *testing.T) {
	src := readScanScript(t)
	// The trivy invocation is split across backslash-continued lines.
	joined := strings.ReplaceAll(src, "\\\n", " ")

	severityRe := regexp.MustCompile(`--severity\s+([A-Z][A-Z,]*)`)
	ms := severityRe.FindStringSubmatch(joined)
	if ms == nil {
		t.Fatalf("--severity flag not found in %s; the parity guard parsed nothing", scanScriptPath)
	}
	scriptSeverity := ms[1]
	if got := strings.Join(DefaultScanSeverity, ","); got != scriptSeverity {
		t.Errorf("DefaultScanSeverity renders %q but %s uses --severity %s; "+
			"hack/scan-images.sh and pkg/foreman/agent/tools/scan_targets.go must change together",
			got, scanScriptPath, scriptSeverity)
	}
	if !DefaultScanIgnoreUnfixed && strings.Contains(joined, "--ignore-unfixed") {
		t.Errorf("%s passes --ignore-unfixed but DefaultScanIgnoreUnfixed is false; "+
			"update scan_targets.go or the script", scanScriptPath)
	}
	if !DefaultScanIgnoreUnfixed {
		t.Errorf("DefaultScanIgnoreUnfixed must be true to match %s's --ignore-unfixed", scanScriptPath)
	}
	for _, re := range [][2]string{
		{`--exit-code\s+(\d+)`, "1"},
		{`--format\s+(\w+)`, "table"},
	} {
		m := regexp.MustCompile(re[0]).FindStringSubmatch(joined)
		if m == nil || m[1] != re[1] {
			t.Errorf("%s must invoke trivy with --%s (got %v); the scan tool's template hard-codes that contract",
				scanScriptPath, re[0], m)
		}
	}

	// The builder stages each binary with the script's build flags.
	const buildFlags = "CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath"
	if !strings.Contains(joined, buildFlags) {
		t.Fatalf("%s no longer builds with %q; update the scan Job template to match",
			scanScriptPath, buildFlags)
	}

	// ... and the rendered default Job agrees with the script on every
	// point compared above.
	targets, err := resolveScanTargets(nil)
	if err != nil {
		t.Fatalf("resolveScanTargets(nil): %v", err)
	}
	job, err := renderScanJob(scanRendererInput{
		Name:                    "foreman-scan-parity",
		Namespace:               "foreman-system",
		BuilderImage:            "golang:1.26",
		RunnerImage:             "alpine:3.24",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "main",
		Targets:                 targets,
		Severity:                resolveScanSeverity(nil),
		IgnoreUnfixed:           DefaultScanIgnoreUnfixed,
		TrivyVersion:            TrivyVersion,
		ActiveDeadlineSeconds:   3600,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
	})
	if err != nil {
		t.Fatalf("renderScanJob: %v", err)
	}
	rendered := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	renderedInit := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, "\n")
	for _, want := range []string{
		"--severity " + scriptSeverity,
		"--ignore-unfixed",
		"--exit-code 1",
		"--format table",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("default scan Job does not render %q (script contract from %s):\n%s",
				want, scanScriptPath, rendered)
		}
	}
	if !strings.Contains(renderedInit, buildFlags) {
		t.Errorf("default scan Job builder does not render %q (script contract from %s):\n%s",
			buildFlags, scanScriptPath, renderedInit)
	}
}

// --- resolveScanTargets unit coverage ---------------------------------------

func TestResolveScanTargets(t *testing.T) {
	t.Run("nil means all, in script order", func(t *testing.T) {
		got, err := resolveScanTargets(nil)
		if err != nil {
			t.Fatalf("resolveScanTargets(nil): %v", err)
		}
		if !reflect.DeepEqual(got, builtInScanTargets) {
			t.Errorf("nil: want all built-ins, got %+v", got)
		}
	})

	t.Run("empty means all", func(t *testing.T) {
		got, err := resolveScanTargets([]string{})
		if err != nil {
			t.Fatalf("resolveScanTargets(empty): %v", err)
		}
		if len(got) != len(builtInScanTargets) {
			t.Errorf("empty: want all built-ins, got %+v", got)
		}
	})

	t.Run("returns a copy", func(t *testing.T) {
		got, err := resolveScanTargets(nil)
		if err != nil {
			t.Fatalf("resolveScanTargets(nil): %v", err)
		}
		got[0].ID = "mutated"
		if builtInScanTargets[0].ID == "mutated" {
			t.Error("resolveScanTargets handed out the built-in table itself, not a copy")
		}
	})

	t.Run("named subset comes back in script order, deduplicated", func(t *testing.T) {
		got, err := resolveScanTargets([]string{"foreman-agent", "controller", "foreman-agent"})
		if err != nil {
			t.Fatalf("resolveScanTargets: %v", err)
		}
		ids := make([]string, 0, len(got))
		for _, tgt := range got {
			ids = append(ids, tgt.ID)
		}
		if want := []string{"controller", "foreman-agent"}; !reflect.DeepEqual(ids, want) {
			t.Errorf("want %v in script order, got %v", want, ids)
		}
	})

	t.Run("unknown id errors naming it", func(t *testing.T) {
		_, err := resolveScanTargets([]string{"controller", "no-such-target"})
		if err == nil || !strings.Contains(err.Error(), `"no-such-target"`) {
			t.Errorf("want error naming the unknown id; got %v", err)
		}
	})

	t.Run("ids are case-sensitive", func(t *testing.T) {
		if _, err := resolveScanTargets([]string{"Controller"}); err == nil {
			t.Error(`"Controller" must not match "controller"`)
		}
	})
}

func TestDefaultScanTargetIDsReturnsCopy(t *testing.T) {
	first := DefaultScanTargetIDs()
	if len(first) > 1 {
		first[0], first[1] = first[1], first[0]
	}
	second := DefaultScanTargetIDs()
	want := []string{"controller", "foreman-operator", "foreman-agent", "router-proxy"}
	if !reflect.DeepEqual(second, want) {
		t.Errorf("DefaultScanTargetIDs leaked a mutable view: %v", second)
	}
}

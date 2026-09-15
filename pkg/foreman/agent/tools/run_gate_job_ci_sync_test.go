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
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflowDir is the CI definition this gate is meant to mirror.
const workflowDir = "../../../../.github/workflows"

// workflowGlob is deliberately *.y*ml and not the tidier *.yml. GitHub
// honours .yaml and .yml equally, so a workflow named .yaml runs its
// checks exactly like any other. Narrowing this pattern does not fail
// anything: it makes that entire file invisible to the tests below, which
// keep passing while covering less CI than they claim to. If you are here
// to simplify it, that is the failure you would be introducing.
const workflowGlob = "*.y*ml"

// gateExemptCIChecks are make targets CI runs that DefaultGateChecks
// deliberately omits. Each entry is a DECISION with a stated reason, not
// a formality: an unexplained omission is how a gate quietly stops
// meaning what its name says (#1637).
var gateExemptCIChecks = map[string]string{
	"validate-samples": "hard-exits when the third-party python3 modules jsonschema and pyyaml are " +
		"absent; python3 itself IS in the gate image, but it is a plain golang image with no pip3 " +
		"to add them, so adding this target would fail every gate run",
	"check-helm-rbac": "scripts/check-helm-rbac.sh exits 2 unless `python3 -c 'import yaml'` " +
		"succeeds; the gate image is a plain golang image that ships python3 but neither PyYAML " +
		"nor pip3, so adding this target would fail every gate run. CI clears it only because " +
		"helm-chart.yml installs PyYAML in a step of its own beforehand",
	"test-envtest": "the CI-only second-seed envtest pass (#1693); the gate deliberately runs " +
		"`make test` single-pass, so gating this target would double the ordering coverage the " +
		"gate was decided not to pay for (see the DefaultGateChecks comment)",
	"test-b200-harness": "the B200 validation harness self-test hard-requires jq " +
		"(test/e2e/b200/lib/capture.sh normalizes `llmkube benchmark` JSON with it), and " +
		"gate_job_template.yaml provisions only helm for the checks that need it, so the gate " +
		"would fail on `required command not found: jq`. Same shape as validate-samples and " +
		"check-helm-rbac. To gate it instead, provision jq in the template the way helm is " +
		"provisioned, then move this target into DefaultGateChecks.",
	"setup-test-e2e":                "provisions an e2e cluster; lifecycle, not a branch check",
	"cleanup-test-e2e":              "tears down an e2e cluster; lifecycle, not a branch check",
	"test-e2e":                      "needs a live cluster the gate Job does not own",
	"docker-build":                  "image build, not a branch check",
	"docker-build-foreman-agent":    "image build, not a branch check",
	"docker-build-foreman-operator": "image build, not a branch check",
}

// golangciConfigTargets maps a golangci-lint --config= value used by a
// workflow to the make target that runs the same check, so a
// GitHub-Action-invoked lint is compared against the gate like any other.
//
// Only a config some workflow actually names with --config= belongs
// here. lint.yml's plain lint step passes no args at all, so an entry
// for .golangci.yml would translate nothing and could never be caught
// being wrong. TestGolangciConfigTargetsAreLive holds that line.
var golangciConfigTargets = map[string]string{
	".golangci-deadcode.yml": "lint-deadcode",
}

var (
	// A make invocation is either `run: make ...` on one line, or a bare
	// `make ...` line inside a `run: |` block. Both forms are live in
	// this repo (test.yml runs `make test` inside a block), and matching
	// only the first would under-report the gap this test exists to
	// close.
	makeLineRe  = regexp.MustCompile(`^(?:-\s*)?(?:run:\s*)?make\s+(.*)$`)
	golangciRe  = regexp.MustCompile(`--config=(\S+)`)
	makeTargetR = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// makeTargetsIn returns the target names from a make invocation's
// argument tail, stopping at the first token that is a variable
// assignment or otherwise not a target (e.g. `IMG="${IMG}"`).
func makeTargetsIn(tail string) []string {
	var out []string
	for _, tok := range strings.Fields(tail) {
		if !makeTargetR.MatchString(tok) {
			break
		}
		out = append(out, tok)
	}
	return out
}

// workflowLine is one non-comment line of one workflow file, tagged with
// the file it came from so a failure can name the workflow.
type workflowLine struct {
	workflow string
	text     string
}

// workflowLines reads every workflow file once and returns their
// non-comment lines. One reader and one glob for every test in this
// file: a test that widened its own view of CI would otherwise leave the
// others reading a smaller CI than the one that runs.
func workflowLines(t *testing.T) []workflowLine {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(workflowDir, workflowGlob))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no workflow files under %s (err=%v)", workflowDir, err)
	}
	var out []workflowLine
	for _, f := range entries {
		buf, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		base := filepath.Base(f)
		for _, line := range strings.Split(string(buf), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments mention targets in prose ("the tests make the code
			// look used", "Run 'make generate ...'"); they invoke nothing.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			out = append(out, workflowLine{workflow: base, text: trimmed})
		}
	}
	return out
}

// ciBlockingChecks returns every make target CI invokes across all
// workflow files, plus the make-target equivalent of any golangci-lint
// action run with an explicit config.
//
// It sees make targets and golangci configs, and nothing else. A CI step
// that runs a tool directly (security.yml's `run: govulncheck ./...`, or
// helm-chart.yml's helm and ct steps) is invisible here, so every test
// built on this function bounds its claim to the make-invoked subset.
func ciBlockingChecks(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, wl := range workflowLines(t) {
		if m := makeLineRe.FindStringSubmatch(wl.text); m != nil {
			for _, target := range makeTargetsIn(m[1]) {
				found[target] = wl.workflow
			}
		}
		for _, m := range golangciRe.FindAllStringSubmatch(wl.text, -1) {
			if target, ok := golangciConfigTargets[m[1]]; ok {
				found[target] = wl.workflow
			}
		}
	}
	return found
}

// TestDefaultGateChecksCoverCI pins the Foreman gate against the
// make-invoked subset of CI. A GATE-PASS is read by operators, and by the
// verdict machinery, as "this branch is expected to pass CI"; that is
// only true while the gate's check list covers the CI checks this test
// can see. Checks CI runs as direct `run:` steps are outside its reach,
// so a green run here is not a promise the pull request is green.
func TestDefaultGateChecksCoverCI(t *testing.T) {
	gate := map[string]bool{}
	for _, c := range DefaultGateChecks {
		gate[c] = true
	}

	for target, workflow := range ciBlockingChecks(t) {
		if gate[target] {
			continue
		}
		if reason, ok := gateExemptCIChecks[target]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%q is exempt from DefaultGateChecks with an empty reason; state why", target)
			}
			continue
		}
		t.Errorf("CI (%s) blocks on `make %s` but DefaultGateChecks does not run it: "+
			"a branch can earn GATE-PASS and still fail CI. Add it to DefaultGateChecks, "+
			"or add it to gateExemptCIChecks with a reason.", workflow, target)
	}
}

// TestGateExemptionsAreLive stops the exemption list from outliving the
// CI steps it excuses. A stale entry silently widens the next real gap.
//
// It is load-bearing for a second reason that is easy to destroy while
// tidying up. docker-build, docker-build-foreman-agent,
// docker-build-foreman-operator, test-e2e and setup-test-e2e appear in
// the workflows ONLY as bare lines inside a `run: |` block, so they are
// reachable only through makeLineRe's optional `run:` prefix. Drop that
// half of the pattern and ciBlockingChecks returns a smaller CI:
// TestDefaultGateChecksCoverCI would still pass, on a gate that had
// quietly stopped looking. This test fails loudly instead. Keep the
// block form when simplifying the matcher.
func TestGateExemptionsAreLive(t *testing.T) {
	ci := ciBlockingChecks(t)
	for target := range gateExemptCIChecks {
		if _, ok := ci[target]; !ok {
			t.Errorf("gateExemptCIChecks has %q but no workflow runs it; drop the stale exemption", target)
		}
	}
}

// TestGateChecksAndExemptionsAreDisjoint catches the inverse of the gap
// TestDefaultGateChecksCoverCI looks for. That test only asks whether a CI
// target is missing from the gate, so a target listed in BOTH
// DefaultGateChecks and gateExemptCIChecks passes every test in this file
// while the two lists say opposite things about it: one runs the check,
// the other records a reason it cannot be run. That is the same shape of
// bug as #1637 itself, a gate configuration that reads plausibly and is
// wrong with nothing to catch it. check-helm-rbac is the live example: it
// is exempt because the gate image has no PyYAML, and adding it back to
// DefaultGateChecks without removing the exemption would turn every gate
// run red again, silently.
func TestGateChecksAndExemptionsAreDisjoint(t *testing.T) {
	for _, c := range DefaultGateChecks {
		if reason, ok := gateExemptCIChecks[c]; ok {
			t.Errorf("%q is in DefaultGateChecks AND in gateExemptCIChecks (%q): a target is "+
				"either gated or exempt, never both. Run it and drop the exemption, or keep the "+
				"exemption and drop it from DefaultGateChecks.", c, reason)
		}
	}
}

// TestGolangciConfigTargetsAreLive applies TestGateExemptionsAreLive's
// rule to the other hand-maintained map. An entry whose config no
// workflow passes with --config= translates nothing: it cannot make the
// coverage test stricter and it can never be caught being wrong, so it
// reads as coverage these tests do not have. .golangci.yml was exactly
// that entry until #1642, because lint.yml runs the linter with no args.
func TestGolangciConfigTargetsAreLive(t *testing.T) {
	seen := map[string]bool{}
	for _, wl := range workflowLines(t) {
		for _, m := range golangciRe.FindAllStringSubmatch(wl.text, -1) {
			seen[m[1]] = true
		}
	}
	for cfg := range golangciConfigTargets {
		if !seen[cfg] {
			t.Errorf("golangciConfigTargets maps %q but no workflow passes --config=%s; "+
				"the entry matches nothing, drop it", cfg, cfg)
		}
	}
}

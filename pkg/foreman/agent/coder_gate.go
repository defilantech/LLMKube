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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/grounding"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
)

// envtestPackagePrefixes are workspace-relative package path prefixes whose
// tests require KUBEBUILDER_ASSETS (envtest) or a live cluster, which the
// coder workspace does not have. The fast gate's unit-test tier skips them
// (running them hangs); CI runs them separately.
var envtestPackagePrefixes = []string{
	"internal/controller/",
	"internal/foreman/controller/",
	"test/",
}

// maxCheckOutputBytes bounds the captured output included in the gate
// feedback for each failing check, so a noisy compiler or linter cannot
// produce an unbounded user message. Output longer than this is truncated.
const maxCheckOutputBytes = 8 * 1024

// commandRunner runs one command in dir with extra env vars (KEY=VALUE)
// appended to the process environment, returning combined stdout+stderr
// and the exec error. Injectable so tests do not shell out.
type commandRunner func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (output string, err error)

// execCommandRunner is the production commandRunner backed by os/exec. It
// appends extraEnv to the inherited process environment and captures
// combined stdout+stderr. Wired into the coder agent loop via
// makeCoderGateVerifier as the runner passed to RunCoderGate.
var execCommandRunner commandRunner = func(
	ctx context.Context,
	dir string,
	extraEnv []string,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkFailure records a single failing verification check and the output
// that explains the failure.
type checkFailure struct {
	name   string
	output string
}

// RunCoderGate runs the fast in-workspace verification tier against a
// coder's workspace and reports whether every check passed. On failure,
// feedback is a directive the agent loop injects as a user message:
// it names the failing check(s) and includes their output so the model
// can fix the issue and resubmit. golangciPath is the path to the
// golangci-lint binary (e.g. "./bin/golangci-lint"). run is the command
// runner (production callers pass execCommandRunner). issueText is the
// query the scope-overlap check ranks files against; an empty string
// disables that check (backward compatible).
//
// The gate runs eleven deterministic checks in order: gofmt, go vet,
// go build, golangci-lint, a fast unit-test tier on changed packages,
// a codegen-drift check, a goreleaser-config check (path-scoped),
// a scope-overlap check, a test-presence check, a mutation-survival check,
// and a reference-grounding check on added docs. Heavy envtest or
// integration tests are intentionally out of scope; they run in a separate
// post-push gate Job. All checks run regardless of earlier failures so the
// feedback reports everything wrong at once.
//
// advisories is a slice of non-blocking findings from the tiered registry.
// It is empty until later tasks add checks to gateCheckRegistry.
func RunCoderGate(
	ctx context.Context,
	workspace, golangciPath string,
	run commandRunner,
	issueText string,
) (pass bool, feedback string, advisories []advisory) {
	var failures []checkFailure

	// 1. gofmt -l . lists misformatted files on stdout and exits 0 even
	// when files are listed, so the failure signal is non-empty output,
	// not the exec error.
	if out, _ := run(ctx, workspace, nil, "gofmt", "-l", "."); strings.TrimSpace(out) != "" {
		failures = append(failures, checkFailure{name: "gofmt -l .", output: out})
	}

	// 2. go vet ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "vet", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go vet ./...", output: out})
	}

	// 3. go build ./... fails with a non-nil error.
	if out, err := run(ctx, workspace, nil, "go", "build", "./..."); err != nil {
		failures = append(failures, checkFailure{name: "go build ./...", output: out})
	}

	// 4. golangci-lint run ./... fails with a non-nil error. GOOS=linux is
	// required: on macOS, plain lint silently skips //go:build !darwin
	// files and would not match CI. GOLANGCI_LINT_CACHE is scoped to a
	// per-workspace sibling directory so stale analysis results from
	// another coder workspace cannot pollute this run's lint (#759); the
	// sibling location keeps the cache out of the workspace git tree.
	lintEnv := []string{"GOOS=linux", "GOLANGCI_LINT_CACHE=" + workspace + ".golangci-cache"}
	if out, err := run(ctx, workspace, lintEnv, golangciPath, "run", "./..."); err != nil {
		failures = append(failures, checkFailure{name: golangciPath + " run ./...", output: out})
	}

	// 5. Fast unit-test tier: go test on the non-envtest packages the coder
	// changed. The static checks above cannot catch a failing or panicking
	// unit test, so a broken test would otherwise reach a GO and only fail
	// in CI (#762). Envtest/integration packages are excluded (they need
	// KUBEBUILDER_ASSETS / a cluster the workspace lacks; CI runs them).
	if pkgs := changedTestPackages(ctx, workspace, run); len(pkgs) > 0 {
		args := append([]string{"test", "-count=1", "-timeout=180s"}, pkgs...)
		if out, err := run(ctx, workspace, nil, "go", args...); err != nil {
			failures = append(failures, checkFailure{name: "go test " + strings.Join(pkgs, " "), output: out})
		}
	}

	// 6. Codegen drift: regenerate manifests/CRDs/deepcopy and deterministically
	// resolve any drift confined to generated artifacts, rather than failing the
	// coder on a mechanical step it was already told to run (#851). Regenerated
	// generated files are left in the workspace and committed by the executor's
	// `git add -A` (repo.Commit). Only a make error, or regeneration touching a
	// NON-generated file, is reported as a failure. Skipped gracefully if
	// controller-gen is unavailable. (Originally a hard drift check, #775.)
	if failed, out := resolveCodegenDrift(ctx, workspace, run); failed {
		failures = append(failures, checkFailure{name: "codegen drift", output: out})
	}

	// 7. Goreleaser config check: when .goreleaser.yaml or any
	// Dockerfile*.goreleaser is changed, run `goreleaser check` to
	// validate the release config schema. This catches broken release
	// configs before they reach a GO (see #854 / #868). Path-scoped so
	// it adds no latency to the common (Go-only) change. Skipped
	// gracefully if goreleaser is not available.
	if failed, out := checkGoreleaserConfig(ctx, workspace, run); failed {
		failures = append(failures, checkFailure{name: "goreleaser check", output: out})
	}

	// 8. Scope-overlap check: flag a coder whose changed Go files have zero
	// overlap with the files the issue implies are relevant, which catches a
	// drift to an unrelated subsystem that every other check happily
	// green-lights (e.g. refactoring pkg/cli/cache.go for a pkg/agent issue).
	// Conservative by design: it only judges non-test Go changes, only fires
	// on near-zero overlap, and stays silent when the issue gives no Go signal
	// (#782).
	if drift, out := checkScopeOverlap(ctx, workspace, run, issueText); drift {
		failures = append(failures, checkFailure{name: "scope overlap", output: out})
	}

	// 9. Test-presence: a change that adds new functions must come with a test
	// in their package. Pure diff inspection, so it covers controller/envtest
	// packages the unit-test tier above cannot run (catches the #856 class:
	// new logic, zero tests). Disabled by FOREMAN_MUTATION_GATE=0.
	if !mutationGateDisabled() {
		if failed, out := checkTestPresence(ctx, workspace, run); failed {
			failures = append(failures, checkFailure{name: "test presence", output: out})
		}
	}

	// 10. Neuter-survival: the changed code must actually be tested. For each
	// non-envtest changed package that has a changed test, blank the changed
	// function bodies on a backed-up copy and re-run the package's tests; if
	// they still pass, the tests do not bite. Restored always. Controller/
	// envtest packages are handled in the post-push gate Job (v1.1).
	if !mutationGateDisabled() {
		if failed, out := checkMutationSurvival(ctx, workspace, run); failed {
			failures = append(failures, checkFailure{name: "mutation survival", output: out})
		}
	}

	// 11. Reference grounding: every LLMKube-owned API group / CRD kind /
	// spec field / metric / CLI command referenced in ADDED doc or example
	// YAML lines must resolve to a real symbol in the repo. Catches the
	// "confabulated reference" class (invented API group, field, metric, or
	// CLI command) that no compiler or linter touches because docs are never
	// built. LLMKube-owned symbols only; external APIs are never judged.
	// Fail-open: a ground-truth load or diff error skips the check.
	if failed, out := checkReferenceGrounding(ctx, workspace, run); failed {
		failures = append(failures, checkFailure{name: "reference grounding", output: out})
	}

	blocking, adv := runGateChecks(ctx, workspace, run, gateCheckRegistry(issueText))
	failures = append(failures, blocking...)
	advisories = adv

	if len(failures) == 0 {
		return true, "", advisories
	}

	return false, buildFeedback(failures), advisories
}

// gateCheckRegistry returns the tiered checks added by the gate-check suite.
// issueText is threaded for checks that need it.
func gateCheckRegistry(issueText string) []gateCheck {
	_ = issueText // used by later tasks
	return []gateCheck{
		{
			name: "rbac-use",
			tier: tierBlock,
			lang: foremanv1alpha1.GateLanguageGo,
			fn:   checkRBACUse,
		},
		{
			name: "import-graph",
			tier: tierBlock,
			lang: foremanv1alpha1.GateLanguageGo,
			fn:   checkImportGraph,
		},
		{
			name: "embedded-artifact",
			tier: tierBlock,
			fn:   checkEmbeddedArtifacts,
		},
		{
			name: "grounding-breadth",
			tier: tierAdvisory,
			lang: foremanv1alpha1.GateLanguageGo,
			fn:   checkGroundingBreadth,
		},
	}
}

// changedPackages returns the workspace-relative Go package directories
// (as "./<dir>/" patterns) that have uncommitted changes per
// `git status -z`. It dedups packages and ignores non-Go files and
// root-level (package main) changes. A git error yields no packages
// (the tier is skipped rather than failing the gate spuriously).
// NUL-terminated output is used so filenames with embedded newlines
// are handled correctly.
func changedPackages(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "-z")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var pkgs []string
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.Fields(strings.TrimSpace(entry))
		if len(fields) == 0 {
			continue
		}
		// porcelain is "XY <path>" (renames end with "-> <new>"); the
		// final field is the current path.
		path := fields[len(fields)-1]
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		dir := filepath.Dir(path)
		if dir == "." {
			continue // root package main; not part of the unit-test tier
		}
		dirKey := dir + "/"
		if seen[dirKey] {
			continue
		}
		seen[dirKey] = true
		pkgs = append(pkgs, "./"+dirKey)
	}
	return pkgs
}

// changedTestPackages returns the workspace-relative Go package directories
// (as "./<dir>/" patterns) that have uncommitted changes per
// `git status -z` and are not envtest-backed. It dedups packages and
// ignores non-Go files and root-level (package main) changes. A git
// error yields no packages (the tier is skipped rather than failing the
// gate spuriously). NUL-terminated output is used so filenames with
// embedded newlines are handled correctly.
func changedTestPackages(ctx context.Context, workspace string, run commandRunner) []string {
	all := changedPackages(ctx, workspace, run)
	var pkgs []string
	for _, p := range all {
		if isEnvtestPackage(p) {
			continue
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// changedEnvtestPackages returns the workspace-relative Go package
// directories (as "./<dir>/" patterns) that have uncommitted changes
// per `git status -z` and DO match envtestPackagePrefixes. These are
// the packages whose tests require KUBEBUILDER_ASSETS and must be
// verified in a clean-room gate Job.
func changedEnvtestPackages(ctx context.Context, workspace string, run commandRunner) []string {
	all := changedPackages(ctx, workspace, run)
	var pkgs []string
	for _, p := range all {
		if !isEnvtestPackage(p) {
			continue
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// isEnvtestPackage reports whether the given package path (e.g.
// "./internal/controller/") matches any envtestPackagePrefix.
func isEnvtestPackage(pkg string) bool {
	for _, pfx := range envtestPackagePrefixes {
		if strings.HasPrefix(pkg, "./"+pfx) {
			return true
		}
	}
	return false
}

// resolveCodegenDrift runs the full code-generation set and deterministically
// resolves codegen drift instead of asking the model to (#851). It regenerates
// manifests, deepcopy, and the Helm chart CRDs; any resulting changes confined
// to generated artifacts are left in the workspace for the executor's
// `git add -A` commit to absorb, so an API change lands with its generated
// files in sync without burning gate-fix attempts on a mechanical step.
//
// It returns (failed, output). failed is true only when `make` itself errors,
// or when regeneration changed a NON-generated file (an anomaly worth
// surfacing, e.g. a hand-edit to a generated file or a generator that rewrote
// source). Drift confined to generated artifacts returns (false, "").
//
// To distinguish what regeneration changed from the coder's own uncommitted
// edits, it snapshots the dirty set before and after `make`: only paths newly
// dirtied by `make` are evaluated. Skipped (returns false, "") if
// bin/controller-gen is not present in the workspace.
func resolveCodegenDrift(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	controllerGen := filepath.Join(workspace, "bin", "controller-gen")
	if _, err := run(ctx, workspace, nil, "test", "-f", controllerGen); err != nil {
		// controller-gen not available; skip gracefully.
		return false, ""
	}

	before := dirtyPathSet(ctx, workspace, run)

	// Regenerate manifests, deepcopy, and sync CRDs to both Helm charts. The
	// foreman CRDs need the separate foreman-chart-crds target; `generate`
	// (deepcopy) is included so an API change that only alters zz_generated
	// does not slip through to CI.
	out, err := run(ctx, workspace, nil,
		"make", "manifests", "generate", "chart-crds", "foreman-chart-crds")
	if err != nil {
		return true, "make manifests generate chart-crds foreman-chart-crds failed:\n" + out
	}

	after := dirtyPathSet(ctx, workspace, run)

	// Files newly dirtied by regeneration (not part of the coder's own edits).
	var unexpected []string
	for path := range after {
		if before[path] {
			continue
		}
		if !isGeneratedArtifact(path) {
			unexpected = append(unexpected, path)
		}
	}

	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		msg := "Regeneration changed files that are not generated artifacts. This usually " +
			"means a generated file was hand-edited, or a source change needs review. " +
			"Inspect and resolve these, then resubmit:\n\n" +
			strings.Join(unexpected, "\n") + "\n"
		return true, msg
	}

	// Any drift was confined to generated artifacts: regenerated files stay in
	// the workspace and are committed by the executor's `git add -A`. Nothing
	// for the model to do.
	return false, ""
}

// dirtyPathSet returns the set of workspace-relative paths reported dirty by
// `git status --porcelain` (modified, untracked, or staged). Rename entries
// ("orig -> new") resolve to the new path. A git error yields an empty set so
// the caller degrades to "no drift detected" rather than failing spuriously.
func dirtyPathSet(ctx context.Context, workspace string, run commandRunner) map[string]bool {
	out, err := run(ctx, workspace, nil, "git", "status", "--porcelain")
	if err != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		// porcelain v1 lines are "XY <path>": 2 status chars + a space, then
		// the path. Shorter lines (e.g. a trailing empty line) are skipped.
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+len(" -> "):]
		}
		if path = strings.TrimSpace(path); path != "" {
			set[path] = true
		}
	}
	return set
}

// isGeneratedArtifact reports whether a workspace-relative path is a
// code-generation output produced by `make manifests generate chart-crds
// foreman-chart-crds` (controller-gen CRDs/RBAC, deepcopy, and the synced Helm
// chart CRDs). The allowlist is deliberately tight so that regeneration
// touching anything outside it is surfaced rather than silently absorbed.
func isGeneratedArtifact(path string) bool {
	switch {
	case strings.HasPrefix(path, "config/crd/"):
		return true
	case path == "config/rbac/role.yaml":
		return true
	case strings.HasPrefix(path, "config/webhook/"):
		return true
	case strings.HasPrefix(path, "charts/llmkube/templates/crds/"):
		return true
	case strings.HasPrefix(path, "charts/foreman/templates/crds/"):
		return true
	case strings.Contains(path, "zz_generated."):
		return true
	}
	return false
}

// releaseConfigChanged reports whether any release-config file is in the
// dirty set: .goreleaser.yaml or any Dockerfile*.goreleaser.
func releaseConfigChanged(dirty map[string]bool) bool {
	for path := range dirty {
		if path == ".goreleaser.yaml" {
			return true
		}
		if strings.HasPrefix(path, "Dockerfile.") && strings.HasSuffix(path, ".goreleaser") {
			return true
		}
	}
	return false
}

// checkGoreleaserConfig runs `goreleaser check` when release-config files
// are changed, returning (failed, output). It is skipped gracefully if
// goreleaser is not available (command not found).
func checkGoreleaserConfig(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	dirty := dirtyPathSet(ctx, workspace, run)
	if !releaseConfigChanged(dirty) {
		return false, ""
	}

	// Check if goreleaser is available; skip gracefully if not.
	if _, err := run(ctx, workspace, nil, "which", "goreleaser"); err != nil {
		return false, ""
	}

	out, err := run(ctx, workspace, nil, "goreleaser", "check")
	if err != nil {
		return true, "goreleaser check failed:\n" + out
	}
	return false, ""
}

// buildFeedback renders the directive and a per-check section for every
// failing check, truncating each check's output to maxCheckOutputBytes.
func buildFeedback(failures []checkFailure) string {
	var b strings.Builder
	b.WriteString("The verification gate failed. Fix the issues below and resubmit.\n")
	for _, f := range failures {
		b.WriteString("\n## ")
		b.WriteString(f.name)
		b.WriteString("\n")
		b.WriteString(truncateOutput(f.output))
		b.WriteString("\n")
	}
	return b.String()
}

// truncateOutput caps output at maxCheckOutputBytes, keeping the tail
// (most recent output) and prefixing a marker when truncation occurs.
func truncateOutput(output string) string {
	if len(output) <= maxCheckOutputBytes {
		return output
	}
	return "...(truncated)...\n" + output[len(output)-maxCheckOutputBytes:]
}

// scopeRelevantTopK bounds how many of the highest-scored files count as
// "relevant" for the scope-overlap check. Generous on purpose: a larger set
// means the check only fires on a clear drift (a change touching none of the
// most-relevant files), keeping false positives low as #782 asks.
const scopeRelevantTopK = 50

// maxRelevantShown caps how many relevant files the scope feedback lists.
const maxRelevantShown = 10

// scopeRelevantFiles returns the workspace's Go files most relevant to
// issueText: up to scopeRelevantTopK files with a positive relevance score
// (the repomap scorer ranks descending, so a non-positive score ends the
// set), as both an ordered slice and a membership set. Injectable so tests
// can supply a canned relevant set without walking the real filesystem.
var scopeRelevantFiles = func(workspace, issueText string) (paths []string, set map[string]bool) {
	files, err := repomap.Walk(workspace, nil)
	if err != nil {
		return nil, nil
	}
	scored := repomap.ScoreFiles(files, issueText)
	set = make(map[string]bool)
	for _, sf := range scored {
		if sf.Score <= 0 || len(paths) >= scopeRelevantTopK {
			break
		}
		paths = append(paths, sf.Path)
		set[sf.Path] = true
	}
	return paths, set
}

// checkScopeOverlap reports whether the coder's diff drifted to an unrelated
// subsystem: its changed non-test Go files have zero overlap with the files
// the issue implies are relevant. It returns (drift, feedback).
//
// It is deliberately conservative to avoid false positives (#782):
//   - issueText empty -> skip (no signal).
//   - no non-test Go files changed -> skip (a yaml/docs-only change is not
//     judged by the Go-aware repomap).
//   - the issue produces no positively-scored Go files -> skip (no Go signal
//     to compare against).
//   - any changed Go file is in the top-K relevant set -> in scope, pass.
//
// Only when there is a real Go signal AND the changed Go files miss it
// entirely is the submit flagged, with feedback naming what changed vs. what
// the issue points at.
func checkScopeOverlap(
	ctx context.Context, workspace string, run commandRunner, issueText string,
) (drift bool, feedback string) {
	if strings.TrimSpace(issueText) == "" {
		return false, ""
	}

	var changedGo []string
	for path := range dirtyPathSet(ctx, workspace, run) {
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			changedGo = append(changedGo, path)
		}
	}
	if len(changedGo) == 0 {
		return false, ""
	}

	relevantPaths, relevantSet := scopeRelevantFiles(workspace, issueText)
	if len(relevantSet) == 0 {
		return false, ""
	}
	for _, p := range changedGo {
		if relevantSet[p] {
			return false, ""
		}
	}

	sort.Strings(changedGo)
	var b strings.Builder
	b.WriteString("Your changed Go files do not overlap with any of the files this issue points at. ")
	b.WriteString("This usually means the change drifted to the wrong subsystem. ")
	b.WriteString("Re-read the issue and edit the relevant files, or explain why these are correct.\n\n")
	b.WriteString("Changed (non-test) Go files:\n")
	for _, c := range changedGo {
		b.WriteString("  - " + c + "\n")
	}
	b.WriteString("\nFiles most relevant to the issue:\n")
	shown := relevantPaths
	if len(shown) > maxRelevantShown {
		shown = shown[:maxRelevantShown]
	}
	for _, r := range shown {
		b.WriteString("  - " + r + "\n")
	}
	if len(relevantPaths) > len(shown) {
		b.WriteString(fmt.Sprintf("  ... and %d more\n", len(relevantPaths)-len(shown)))
	}
	return true, b.String()
}

// checkReferenceGrounding loads LLMKube ground truth from the workspace and
// flags any ungrounded LLMKube-owned reference in added .md/.yaml lines.
// Fail-open on any load/diff error so the grounding net never blocks a coder
// on its own failure.
func checkReferenceGrounding(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	gt, err := grounding.LoadGroundTruth(
		filepath.Join(workspace, "config/crd/bases"),
		workspace, // scan the whole repo for llmkube_* metric literals: the
		//            metal-agent metrics live in pkg/agent, not internal/metrics.
		"", // CLI command validation disabled in v1 (prose false-positive risk).
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
	if len(findings) == 0 {
		return false, ""
	}
	var b strings.Builder
	b.WriteString("These docs reference LLMKube symbols that do not exist." +
		" Fix the reference (or the code) so it resolves:\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "  - %s\n", f.String())
	}
	return true, b.String()
}

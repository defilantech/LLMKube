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

// Package-local mutation gate: two deterministic coder-gate checks that catch
// changes which pass the syntactic checks (gofmt/vet/build/lint) but are not
// actually constrained by a test (the #856 class).
//
//   - Layer 1 (checkTestPresence): a changed package that adds net-new
//     functions in hand-written, non-test Go but has no changed _test.go fails.
//     Pure diff inspection, so it covers envtest/controller packages too.
//   - Layer 2 (checkMutationSurvival): for non-envtest changed packages that DO
//     have a changed test, blank the changed function bodies on an in-memory
//     backup, re-run the package tests, and flag any package whose tests still
//     pass (the tests do not bite). Files are always restored.
//
// Both are disabled by FOREMAN_MUTATION_GATE=0. Neuter-survival for controller
// packages runs in the post-push envtest gate Job (v1.1), not here.
package agent

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// generatedMarker matches the canonical "// Code generated ... DO NOT EDIT."
// line that conventional Go generators emit.
var generatedMarker = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedGoFile reports whether a workspace-relative .go path is generated
// and therefore exempt from the mutation gate's presence and neuter checks.
// Name heuristics catch the common cases without I/O; the DO NOT EDIT marker is
// checked against the file head for the rest. A read error is treated as
// not-generated (the checks then apply, which is the safe default).
func isGeneratedGoFile(workspace, path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "zz_generated.") || strings.HasSuffix(base, ".pb.go") {
		return true
	}
	data, err := os.ReadFile(filepath.Join(workspace, path))
	if err != nil {
		return false
	}
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	return generatedMarker.Match(head)
}

// changedNonTestGoFiles returns workspace-relative paths of changed .go files
// that are neither tests nor generated -- i.e. the hand-written logic files the
// presence and neuter checks judge. Mirrors changedPackages' git porcelain
// parsing. A git error yields nil (checks skip rather than fail spuriously).
func changedNonTestGoFiles(ctx context.Context, workspace string, run commandRunner) []string {
	out, err := run(ctx, workspace, nil, "git", "status", "-z")
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.Fields(strings.TrimSpace(entry))
		if len(fields) == 0 {
			continue
		}
		path := fields[len(fields)-1] // rename "-> new" leaves the new path last
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		if isGeneratedGoFile(workspace, path) {
			continue
		}
		files = append(files, path)
	}
	return files
}

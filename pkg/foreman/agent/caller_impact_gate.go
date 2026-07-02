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

// caller_impact_gate.go implements the caller-impact advisory gate check.
//
// When a coder modifies a shared function, other call sites may silently
// regress even though every other gate check passes. This check surfaces
// those external callers to the reviewer so they can assess blast radius.
//
// This is ADVISORY: findings do not fail the gate loop; they are handed to
// the reviewer via RunCoderGate's returned advisories slice.
package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// maxCallerSites is the most external call sites shown per changed function.
// If more exist, a "(+N more)" note is appended.
const maxCallerSites = 10

// checkCallerImpact is an advisory gate check. For each non-test Go file
// changed in the workspace it identifies functions that were added or body-
// modified, then uses grep to find all call sites in the working tree. If a
// changed function has one or more external callers (i.e. a caller in a file
// other than the definition file) an advisory listing those sites is emitted.
//
// Conservatism rules:
//   - Unexported functions with zero external callers: no advisory.
//   - Exported functions with zero external callers: no advisory.
//   - Grep or diff errors cause the check to skip that function (fail-open).
//
// Returns (true, output) when at least one changed function has external
// callers; (false, "") otherwise.
func checkCallerImpact(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	changed := changedNonTestGoFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}

	// Collect changed function names per definition file.
	type funcDef struct {
		name    string
		defFile string // workspace-relative path
	}
	var candidates []funcDef
	seen := map[string]bool{}

	for _, f := range changed {
		added := addedFuncNames(ctx, workspace, f, run)
		modified := modifiedFuncNames(ctx, workspace, f, run)
		all := append(added, modified...) //nolint:gocritic // intentional extend
		for _, name := range all {
			if name == "" {
				continue
			}
			key := f + ":" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			candidates = append(candidates, funcDef{name: name, defFile: f})
		}
	}

	if len(candidates) == 0 {
		return false, ""
	}

	var b strings.Builder
	anyAdvisory := false

	for _, cd := range candidates {
		callers := externalCallers(ctx, workspace, cd.name, cd.defFile, run)
		if len(callers) == 0 {
			// For unexported functions, no external callers: skip.
			// For exported functions, also skip (nothing else calls it yet).
			continue
		}
		anyAdvisory = true
		shown := callers
		extra := 0
		if len(shown) > maxCallerSites {
			extra = len(shown) - maxCallerSites
			shown = shown[:maxCallerSites]
		}
		callerList := strings.Join(shown, ", ")
		if extra > 0 {
			callerList += fmt.Sprintf(" (+%d more)", extra)
		}
		fmt.Fprintf(&b, "changed %s (%s); callers that may be affected: %s\n",
			cd.name, cd.defFile, callerList)
	}

	if !anyAdvisory {
		return false, ""
	}
	return true, b.String()
}

// externalCallers greps the working tree for call sites of funcName, then
// filters to lines that (a) contain `funcName(` (a call, not just a comment
// or import name) and (b) are in a file other than defFile. This excludes the
// declaration line AND any same-file recursive or chained calls; only
// cross-file callers remain. Returns nil on grep error (fail-open).
func externalCallers(ctx context.Context, workspace, funcName, defFile string, run commandRunner) []string {
	grepOut, err := run(ctx, workspace, nil, "grep", "-rn", "--include=*.go", funcName, ".")
	if err != nil {
		// grep returns exit 1 when there are no matches; treat as no callers.
		return nil
	}

	// The definition line pattern: the file is defFile and the line text
	// contains "func <name>".
	defFileNorm := normalizeGrepPath(defFile)

	seen := map[string]bool{}
	var callers []string

	for _, line := range strings.Split(strings.TrimRight(grepOut, "\n"), "\n") {
		if line == "" {
			continue
		}
		// grep -n output: <path>:<linenum>:<text>
		// Path may start with "./" so we normalise both sides.
		filePart, lineNum, text, ok := parseGrepLine(line)
		if !ok {
			continue
		}
		// Must be an actual call site (a `<funcName>(` token with an identifier
		// boundary on the left, so `New(` does not match inside `Renew(`).
		if !containsCall(text, funcName) {
			continue
		}
		site := filePart + ":" + lineNum

		// Exclude all lines in the definition file (declaration + same-file
		// recursive/chained calls). Only cross-file callers are external.
		if normalizeGrepPath(filePart) == defFileNorm {
			continue
		}

		if seen[site] {
			continue
		}
		seen[site] = true
		callers = append(callers, site)
	}
	return callers
}

// parseGrepLine splits a `grep -rn` output line into (file, linenum, text, ok).
// The format is "<file>:<linenum>:<text>" where file can contain colons on
// Windows; we split on the first two colons only.
func parseGrepLine(line string) (file, linenum, text string, ok bool) {
	first := strings.Index(line, ":")
	if first < 0 {
		return
	}
	rest := line[first+1:]
	second := strings.Index(rest, ":")
	if second < 0 {
		return
	}
	file = line[:first]
	linenum = rest[:second]
	text = rest[second+1:]
	ok = true
	return
}

// normalizeGrepPath strips a leading "./" from a grep-output path so that
// "./pkg/x/y.go" and "pkg/x/y.go" compare equal.
func normalizeGrepPath(p string) string {
	return filepath.Clean(p)
}

// containsCall reports whether text contains a call token `<funcName>(` with an
// identifier boundary on the left, so a short name like `New` is not matched
// inside a longer identifier such as `Renew(`. It scans every occurrence of
// `<funcName>(` and accepts the first one whose preceding byte (if any) is not
// an identifier character.
func containsCall(text, funcName string) bool {
	needle := funcName + "("
	for off := 0; ; {
		idx := strings.Index(text[off:], needle)
		if idx < 0 {
			return false
		}
		start := off + idx
		if start == 0 || !isIdentByte(text[start-1]) {
			return true
		}
		off = start + 1 // keep scanning past this (rejected) occurrence
	}
}

// isIdentByte reports whether b can appear inside a Go identifier.
func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

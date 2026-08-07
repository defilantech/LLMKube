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
)

// checkCommandStringTestDilution is a tierAdvisory gate check (#1346). It
// surfaces to the reviewer when a submission that modifies a shell/exec
// command string in production code adds only string-shape test assertions
// (ContainSubstring, strings.Contains, Equal on a string literal) with no
// behavioral test that executes the produced command or exercises the branch
// at runtime.
//
// Fires when BOTH hold:
//
//	(a) the diff modifies a non-test Go file at a site that builds a shell
//	    or exec command string (exec.Command(, "sh", "-c", or a string
//	    literal with shell metacharacters used as a command), AND
//	(b) the same package has a changed _test.go file whose ADDED assertion
//	    lines are exclusively string-shape assertions.
//
// Never fails the gate; the coder never sees it.
func checkCommandStringTestDilution(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	// Stage the working tree so a pre-commit diff includes new/untracked files.
	if _, err := run(ctx, workspace, nil, "git", "add", "-A"); err != nil {
		return false, ""
	}
	nsOut, err := run(ctx, workspace, nil, "git", "diff", "--name-status", "--cached", "HEAD")
	if err != nil {
		return false, ""
	}
	entries := parseNameStatus(nsOut)
	prodPkgs := changedProdPackages(entries)
	if len(prodPkgs) == 0 {
		return false, ""
	}

	// Get the full diff for all changed files (not just tests).
	//
	// --unified=3 rather than 0: a Go command-construction call is routinely
	// spread over several lines, so with zero context the only thing in the
	// diff is the changed argument, which carries no evidence that it belongs
	// to a command at all. The context lines supply that evidence. Added and
	// removed lines still arrive unpaired, and nothing here assumes otherwise.
	diffOut, err := run(ctx, workspace, nil, "git", "diff", "--cached", "--unified=3",
		"--src-prefix=a/", "--dst-prefix=b/", "HEAD")
	if err != nil {
		return false, ""
	}
	byFile := parseUnifiedDiff(diffOut)

	// (a) Find production packages that changed a command-string site.
	cmdPkgs := commandStringChangedPackages(byFile, prodPkgs)
	if len(cmdPkgs) == 0 {
		return false, ""
	}

	// (b) For each such package, check if the test file's added assertions
	// are exclusively string-shape assertions.
	var findings []string
	for pkg := range cmdPkgs {
		if hasOnlyStringShapeAssertions(byFile, pkg) {
			findings = append(findings, fmt.Sprintf(
				"%s: command-string change with only string-shape test assertions (no behavioral test)", pkg))
		}
	}

	if len(findings) == 0 {
		return false, ""
	}
	detail := "production command-string change detected; added tests only assert string shape, not runtime behavior: " +
		strings.Join(findings, "; ")
	return true, truncateOutput(detail)
}

// commandStringChangedPackages returns the set of production package
// directories whose changed non-test Go files contain command-string
// modifications (exec.Command(, "sh", "-c", or shell metacharacters in a
// command string). Only packages already in prodPkgs are considered.
func commandStringChangedPackages(byFile map[string]*fileHunks, prodPkgs map[string]bool) map[string]bool {
	result := map[string]bool{}
	for file, fh := range byFile {
		dir := filepath.Dir(file)
		if !prodPkgs[dir] {
			continue
		}
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		if hasCommandStringChange(fh) {
			result[dir] = true
		}
	}
	return result
}

// hasCommandStringChange reports whether the diff hunks contain a
// modification to a shell/exec command string, using two detectors.
//
//	Direct: a changed line itself carries exec.Command(, the pair "sh", "-c",
//	or shell metacharacters inside a string literal in command context.
//
//	Enclosing: a changed line is a bare string-literal argument, the shape an
//	argument takes inside a multiline call, AND the surrounding context shows
//	command construction.
//
// The second detector exists because the motivating case (#1309) is invisible
// to the first. Changing `"-w", "%{size_download}"` to `"%{size_total}"` inside
// a multiline exec.Command produces a diff line with no exec.Command(, no
// "sh"/"-c" pair, and no character from shellMeta (note that % and { are not
// shell metacharacters). The gate built to catch #1309 would have missed #1309.
func hasCommandStringChange(fh *fileHunks) bool {
	changed := make([]string, 0, len(fh.Added)+len(fh.Removed))
	changed = append(changed, fh.Added...)
	changed = append(changed, fh.Removed...)

	for _, l := range changed {
		if isCommandStringLine(l) {
			return true
		}
	}

	if !contextHasCommandConstruction(fh.Context) {
		return false
	}
	for _, l := range changed {
		if isStringLiteralArgument(l) {
			return true
		}
	}
	return false
}

// contextHasCommandConstruction reports whether any unchanged context line
// opens a command-construction call. Deliberately narrower than
// isCommandStringLine: context is proximity evidence, not a change, so only
// unambiguous constructors count. hasCommandContext's loose tokens ("cmd.",
// "command") would match far too much surrounding code.
func contextHasCommandConstruction(contextLines []string) bool {
	for _, l := range contextLines {
		if strings.Contains(l, "exec.Command(") || strings.Contains(l, "exec.CommandContext(") {
			return true
		}
		if strings.Contains(l, `"sh"`) && strings.Contains(l, `"-c"`) {
			return true
		}
	}
	return false
}

// isStringLiteralArgument reports whether a line is purely argument-list
// material: one or more double-quoted string literals separated by commas,
// with nothing else but whitespace and an optional closing paren or ellipsis.
// That is what a changed argument inside a multiline call looks like, e.g.
//
//	"-w", "%{size_total}",
//
// Requiring the whole line to be argument-shaped keeps this from matching
// ordinary code that merely contains a string, such as `x := f("a") + b`.
func isStringLiteralArgument(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, `"`) {
		return false
	}
	inQuote := false
	escaped := false
	sawLiteral := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		if inQuote {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inQuote = false
				sawLiteral = true
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
		case ',', ' ', '\t', ')':
			// Separator or a closing paren: still argument-shaped.
		case '.':
			// Tolerate a trailing variadic ellipsis.
		default:
			return false
		}
	}
	return sawLiteral && !inQuote
}

// isCommandStringLine reports whether a diff content line looks like it
// builds a shell or exec command string. It checks for:
//   - exec.Command(
//   - the pair "sh", "-c" (shell invocation)
//   - a string literal containing shell metacharacters ($, |, ;, &, >, <, \`)
//     that is used as a command argument
func isCommandStringLine(s string) bool {
	if strings.Contains(s, "exec.Command(") {
		return true
	}
	// "sh", "-c" pattern: both must appear on the same line.
	if strings.Contains(s, `"sh"`) && strings.Contains(s, `"-c"`) {
		return true
	}
	// Shell metacharacters in a string literal used as a command.
	// Look for a string literal (double-quoted or backtick) containing
	// shell metacharacters that is passed to a command-related function.
	if hasShellMetacharInCommand(s) {
		return true
	}
	return false
}

// hasShellMetacharInCommand reports whether a line contains a string literal
// with shell metacharacters that is used as a command argument. This catches
// cases like `cmd := "curl -I -w '%{size_download}'"` or similar.
func hasShellMetacharInCommand(s string) bool {
	shellMeta := "$|;&><`"
	// Check for string literals containing shell metacharacters.
	// We look for a double-quoted string or backtick string that contains
	// at least one shell metacharacter, and the line references a command
	// or exec-related context.
	if !hasCommandContext(s) {
		return false
	}
	// Check for shell metacharacters inside a string literal.
	inDoubleQuote := false
	inBacktick := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' && !inBacktick:
			inDoubleQuote = !inDoubleQuote
		case c == '`' && !inDoubleQuote:
			inBacktick = !inBacktick
		case (inDoubleQuote || inBacktick) && strings.ContainsRune(shellMeta, rune(c)):
			return true
		}
	}
	return false
}

// hasCommandContext reports whether a line has command-related context
// (references to Command, Run, Output, CombinedOutput, or similar).
func hasCommandContext(s string) bool {
	contexts := []string{
		"Command(", "Run(", "Output(", "CombinedOutput(",
		"exec.", "shell.", "cmd.", "command",
	}
	for _, ctx := range contexts {
		if strings.Contains(s, ctx) {
			return true
		}
	}
	return false
}

// hasOnlyStringShapeAssertions reports whether EVERY changed _test.go file in
// the package that added assertions added only string-shape ones. False when
// the package added no assertions at all (nothing to judge).
//
// The "every file" quantifier is the point. Returning true on the first
// shape-only file would fire on a package that added a real behavioral
// assertion in a sibling file: a package with shape_test.go (ContainSubstring)
// and behavior_test.go (Equal(0) on an exit code) is exactly the case this
// advisory must stay silent for, because the behavior IS covered.
func hasOnlyStringShapeAssertions(byFile map[string]*fileHunks, pkg string) bool {
	sawAssertions := false
	for file, fh := range byFile {
		if filepath.Dir(file) != pkg {
			continue
		}
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		if !hasAddedAssertions(fh) {
			continue
		}
		sawAssertions = true
		if !allAddedAssertionsAreStringShape(fh) {
			return false
		}
	}
	return sawAssertions
}

// hasAddedAssertions reports whether the file hunks have any added
// assertion lines.
func hasAddedAssertions(fh *fileHunks) bool {
	for _, l := range fh.Added {
		if isAssertionLine(l) {
			return true
		}
	}
	return false
}

// allAddedAssertionsAreStringShape reports whether all added assertion
// lines in the file hunks are string-shape assertions (ContainSubstring,
// strings.Contains, or Equal on a string literal). If there are no added
// assertions, returns false (no test to judge).
func allAddedAssertionsAreStringShape(fh *fileHunks) bool {
	hasAssertion := false
	for _, l := range fh.Added {
		if !isAssertionLine(l) {
			continue
		}
		hasAssertion = true
		if !isStringShapeAssertion(l) {
			return false
		}
	}
	return hasAssertion
}

// isStringShapeAssertion reports whether an assertion line is a
// string-shape assertion (as opposed to a behavioral assertion like
// checking exit codes, parsed results, or runtime behavior).
func isStringShapeAssertion(s string) bool {
	t := strings.TrimSpace(s)
	// ContainSubstring is always string-shape.
	if strings.Contains(t, "ContainSubstring(") {
		return true
	}
	// strings.Contains is always string-shape.
	if strings.Contains(t, "strings.Contains(") {
		return true
	}
	// Equal( whose DIRECT argument is a string literal: comparing against a
	// literal string is a shape check, not a behavioral one.
	return isEqualOnStringLiteral(t)
}

// isEqualOnStringLiteral reports whether the line contains an Equal( whose
// first argument opens a string literal, e.g. Equal("draft-simple").
//
// Checking the direct argument rather than counting quotes anywhere on the
// line matters: `Expect(exitCode).To(Equal(0)) // the "fast path" returns zero`
// is a behavioral assertion, but a quote count of two or more reads it as
// string-shape and misclassifies it.
func isEqualOnStringLiteral(s string) bool {
	idx := strings.Index(s, "Equal(")
	if idx < 0 {
		return false
	}
	rest := strings.TrimLeft(s[idx+len("Equal("):], " \t")
	return strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "`")
}

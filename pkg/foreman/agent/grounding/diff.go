package grounding

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// AddedLine is one added line attributed to its new-file path and line number.
type AddedLine struct {
	File string
	Line int
	Text string
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// AddedLines returns the added (+) lines of a WORKING-TREE diff against base
// (`git diff --unified=0 <base> -- pathspec`), each attributed to its new-file
// line number. It first runs `git add -N -- pathspec` (intent-to-add) so newly
// created, still-untracked files are included: the coder gate runs BEFORE any
// commit or stage, so the changes under test live in the working tree and new
// files are untracked. intent-to-add stages no content and is superseded by the
// executor's later `git add -A`. Pass base="HEAD" to capture exactly the
// uncommitted changes on the current branch. A git error from the diff is
// returned; an empty diff yields nil with no error. (Using `<base>...HEAD` here
// would be a bug: at gate time HEAD has no coder commit yet, so committed
// history shows none of the working-tree changes.)
func AddedLines(
	ctx context.Context, workspace string, run CommandRunner, base string, pathspec []string,
) ([]AddedLine, error) {
	// Stage the working-tree changes for these paths so a pre-commit diff
	// includes new (untracked) files: `git add -N` (intent-to-add) is NOT
	// reliably shown by `git diff <base>`, so stage for real with `git add -A`.
	// The executor's later `git add -A` supersedes this, and the workspace is
	// discarded after the run, so the staging is harmless.
	addArgs := append([]string{"add", "-A", "--"}, pathspec...)
	_, _ = run(ctx, workspace, nil, "git", addArgs...) // best-effort

	// Diff the staged changes against base (--cached): new files show as full
	// additions, modified files show their changes. base="HEAD" yields exactly
	// the coder's uncommitted changes on the current branch. --src-prefix=a/
	// --dst-prefix=b/ force the standard a/ b/ path prefixes: `git diff --cached`
	// otherwise emits mnemonic c/ i/ prefixes, which the +++ b/ parser misses.
	args := append([]string{
		"diff", "--cached", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/", base, "--",
	}, pathspec...)
	out, err := run(ctx, workspace, nil, "git", args...)
	if err != nil {
		return nil, err
	}
	var added []AddedLine
	var curFile string
	var curLine int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			curFile = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "): // /dev/null etc.
			curFile = ""
		case strings.HasPrefix(line, "@@"):
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				curLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(line, "+") && curFile != "":
			added = append(added, AddedLine{File: curFile, Line: curLine, Text: strings.TrimPrefix(line, "+")})
			curLine++
		}
	}
	return added, nil
}

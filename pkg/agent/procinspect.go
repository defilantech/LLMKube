package agent

import (
	"path/filepath"
	"slices"
)

// processInspector reads kernel facts about a process that identify it beyond
// its PID. Engine records store the start token so an agent that restarts can
// tell its own engine from an unrelated process that later reused the PID
// (#2079).
type processInspector interface {
	// StartToken returns an opaque value that is equal for the same process
	// across calls and different for any other process that reuses the PID.
	// It fails when no live process has the PID.
	StartToken(pid int) (string, error)
	// Executables returns the candidate paths of the running program,
	// de-duplicated: the kernel exec path, argv[0], and argv[1] (the script
	// when the program is a #! interpreter), each as the kernel reports it and
	// symlink-resolved now. A match against any candidate is a match.
	Executables(pid int) ([]string, error)
}

// executableMatches reports whether any recorded path is one of the running
// program's candidate paths. An empty recorded path never matches.
func executableMatches(candidates []string, recorded ...string) bool {
	for _, r := range recorded {
		if r != "" && slices.Contains(candidates, r) {
			return true
		}
	}
	return false
}

// executableCandidates turns the absolute paths the kernel reports for a
// process into Executables' result: each path as reported and resolved
// through symlinks now, de-duplicated. A bare name like "llama-server" says
// nothing about the binary and is skipped. The unresolved form keeps an
// engine started through a symlink recognizable after the link is repointed.
func executableCandidates(raw []string) []string {
	var out []string
	for _, p := range raw {
		if !filepath.IsAbs(p) {
			continue
		}
		forms := []string{p}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			forms = append(forms, resolved)
		}
		for _, f := range forms {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

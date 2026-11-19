/*
Copyright 2026.

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

// Command check-agents-md validates that every make target and repo path
// referenced by AGENTS.md still resolves, and that its Go version claim matches
// go.mod. AGENTS.md is the file agents read first, so a stale reference there
// sends an agent down a dead end; this check is what keeps it honest.
//
// Usage:
//
//	go run ./cmd/check-agents-md --check
//
// It exits 1 and prints one FAIL line per broken reference.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const agentsDoc = "AGENTS.md"

// refKind distinguishes the two reference classes the check resolves.
type refKind int

const (
	refTarget refKind = iota
	refPath
)

type ref struct {
	kind  refKind
	value string
}

var (
	// makeRef matches a backticked `make <target>` reference.
	makeRef = regexp.MustCompile(`^make ([A-Za-z0-9_-]+)$`)
	// goVersion matches the "Go <major>.<minor>" claim in AGENTS.md.
	goVersion = regexp.MustCompile(`\bGo ([0-9]+\.[0-9]+)`)
	// gomodGo matches the go directive in go.mod.
	gomodGo = regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+)`)
	// makeTarget matches a target definition at the start of a Makefile line.
	makeTarget = regexp.MustCompile(`^([A-Za-z0-9_-]+):`)
)

func main() {
	root := flag.String("root", ".", "repo root to check")
	// --check is accepted so the make target reads like the other drift
	// checks; this checker has no write mode, so it always verifies.
	_ = flag.Bool("check", true, "verify references resolve (always on)")
	flag.Parse()

	doc, err := os.ReadFile(filepath.Join(*root, agentsDoc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-agents-md:", err)
		os.Exit(1)
	}

	problems, err := check(*root, string(doc))
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-agents-md:", err)
		os.Exit(1)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "FAIL: %s\n", p)
		}
		os.Exit(1)
	}
	fmt.Printf("AGENTS.md references resolve (%s)\n", *root)
}

// check returns one human-readable problem per reference in doc that does not
// resolve against root. An empty slice means the document is consistent.
func check(root, doc string) ([]string, error) {
	top, err := topLevel(root)
	if err != nil {
		return nil, err
	}
	targets, err := makeTargets(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}

	var problems []string
	for _, r := range extractRefs(doc, top) {
		switch r.kind {
		case refTarget:
			if !targets[r.value] {
				problems = append(problems, fmt.Sprintf("AGENTS.md references `make %s`; no such target in Makefile", r.value))
			}
		case refPath:
			if _, err := os.Stat(filepath.Join(root, r.value)); err != nil {
				problems = append(problems, fmt.Sprintf("AGENTS.md references path %q; it does not exist", r.value))
			}
		}
	}

	if m := goVersion.FindStringSubmatch(doc); m != nil {
		gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
		if err != nil {
			return nil, err
		}
		if gm := gomodGo.FindStringSubmatch(string(gomod)); gm != nil && gm[1] != m[1] {
			problems = append(problems, fmt.Sprintf("AGENTS.md claims Go %s; go.mod declares Go %s", m[1], gm[1]))
		}
	}
	return problems, nil
}

// extractRefs pulls every backticked `make <target>` and repo path out of doc.
func extractRefs(doc string, top map[string]bool) []ref {
	var refs []ref
	for i := 0; i < len(doc); {
		start := strings.IndexByte(doc[i:], '`')
		if start < 0 {
			break
		}
		start += i
		end := strings.IndexByte(doc[start+1:], '`')
		if end < 0 {
			break
		}
		end += start + 1
		token := strings.TrimSpace(doc[start+1 : end])
		i = end + 1

		if m := makeRef.FindStringSubmatch(token); m != nil {
			refs = append(refs, ref{refTarget, m[1]})
			continue
		}
		if p, ok := repoPath(token, top); ok {
			refs = append(refs, ref{refPath, p})
		}
	}
	return refs
}

// repoPath reports whether a backticked token is a repo path worth resolving.
// Only a token whose first segment is a real top-level entry counts, so label
// keys (`inference.llmkube.dev/model-router`), branch slugs (`feat/<slug>`), and
// build tags (`//go:build`) are ignored rather than reported as missing.
func repoPath(token string, top map[string]bool) (string, bool) {
	if strings.ContainsAny(token, "<>*{}$:?() ") {
		return "", false
	}
	p := strings.TrimSuffix(token, "/")
	slash := strings.IndexByte(p, '/')
	if slash < 1 {
		return "", false
	}
	if !top[p[:slash]] {
		return "", false
	}
	return p, true
}

// topLevel is the set of entries in the repo root, the gate that decides which
// backticked tokens are treated as repo paths.
func topLevel(root string) (map[string]bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}

// makeTargets is the set of target names defined in the Makefile.
func makeTargets(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if m := makeTarget.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out, nil
}

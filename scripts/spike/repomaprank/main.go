//go:build ignore

// Spike collateral (feat/ripwire_spike). Not product code.
//
// Prints the repomap ranking for one query so the ripwire eval driver can
// compare it against ripwire's --for ranking through one scoring path.
// Run with: go run scripts/spike/repomaprank/main.go <workspace> <query>
//
// The //go:build ignore tag keeps this file out of `go build ./...`,
// `go vet ./...` and the release graph; it is invoked explicitly by path.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/repomap"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: repomaprank <workspace> <query>")
		os.Exit(2)
	}
	workspace := os.Args[1]
	query := strings.Join(os.Args[2:], " ")

	files, err := repomap.Walk(workspace, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "walk: %v\n", err)
		os.Exit(1)
	}
	scored := repomap.ScoreFiles(files, query)
	for i, sf := range scored {
		// One line per file, rank order. Score kept so the driver can see
		// the positive/negative boundary the coder gate uses.
		fmt.Printf("%d\t%s\t%g\n", i+1, sf.Path, sf.Score)
	}
}

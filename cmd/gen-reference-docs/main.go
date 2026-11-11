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

// Command gen-reference-docs writes the generated reference pages of the docs
// site: the CLI, Helm values, Prometheus metrics and CRD pages under
// docs/site/reference. Each is rendered from what ships (the cobra tree, the
// charts' values.yaml, the registered collectors, the generated CRD YAML), so
// a change there changes the page, and make check-docs-reference fails on a
// branch that changed one without regenerating the other.
//
// Usage:
//
//	go run ./cmd/gen-reference-docs [-root <repo root>] [-out <dir>]
//
// It reads sources relative to -root and writes the four pages into -out,
// which make check-docs-reference points at a temporary directory.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// page is one generated file and the function that renders it from the repo
// rooted at its argument.
type page struct {
	file   string
	render func(root string) (string, error)
}

var pages = []page{
	{"cli.md", func(string) (string, error) { return renderCLI() }},
	{"helm-values.md", renderHelmValues},
	{"metrics.md", func(string) (string, error) { return renderMetrics() }},
	{"crds.md", renderCRDs},
}

func main() {
	root := flag.String("root", ".", "repository root the sources are read from")
	out := flag.String("out", filepath.Join("docs", "site", "reference"), "directory the pages are written to")
	flag.Parse()

	if err := run(*root, *out); err != nil {
		fmt.Fprintf(os.Stderr, "gen-reference-docs: %v\n", err)
		os.Exit(1)
	}
}

// run renders every page before writing any, so a failure leaves the
// previous pages in place rather than a mix of old and new.
func run(root, out string) error {
	rendered := make([]string, len(pages))
	for i, p := range pages {
		s, err := p.render(root)
		if err != nil {
			return fmt.Errorf("%s: %w", p.file, err)
		}
		rendered[i] = s
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for i, p := range pages {
		if err := os.WriteFile(filepath.Join(out, p.file), []byte(rendered[i]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

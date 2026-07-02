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

// checkImportGraph is a gate check that detects NEW cross-layer import edges
// introduced by a change. It compares the import set in the WORKING TREE (disk)
// against the HEAD import set for each changed non-test Go file and flags
// any import that was not present on HEAD and violates the layering rules.
//
// The only rule today: a package under pkg/ must not newly import a package
// under internal/. Pre-existing edges are not flagged. External and stdlib
// imports are never judged.
//
// The check is fail-open: a disk read or parse error on the current version is
// silently skipped (the build check covers compile errors; we must not block
// WIP that is not yet parseable). A missing HEAD version (new file) is treated
// as an empty import set, so all its imports are considered "new".
package agent

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// llmkubeModule is the module path for this repository. It is used to
// distinguish in-module imports (subject to layering rules) from external
// imports (never flagged).
const llmkubeModule = "github.com/defilantech/llmkube"

// layerRule describes one forbidden cross-layer import pattern.
// fromPrefix and toPrefix are module-relative path prefixes (without the
// module prefix). A violation is emitted when:
//   - the importer's module-relative path starts with fromPrefix, AND
//   - the imported package's module-relative path starts with toPrefix, AND
//   - the edge is NEW (not present on HEAD).
type layerRule struct {
	fromPrefix string
	toPrefix   string
	reason     string
}

// layerRules is the active set of layering constraints. Add entries here to
// extend the check without modifying checkImportGraph itself.
var layerRules = []layerRule{
	{
		fromPrefix: "pkg/",
		toPrefix:   "internal/",
		reason:     "pkg/ packages must not import internal/ (move the shared helper to a neutral package)",
	},
}

// parseImports returns the set of import paths declared in a Go source string.
// It uses parser.ImportsOnly so the full function bodies are not parsed.
// On a parse error it returns nil, false. The path argument is used only for
// error attribution by the parser.
func parseImports(path, src string) (map[string]bool, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if err != nil {
		return nil, false
	}
	imports := make(map[string]bool, len(f.Imports))
	for _, spec := range f.Imports {
		raw := spec.Path.Value
		// ImportSpec.Path.Value is a quoted string literal; unquote it.
		unquoted, err := strconv.Unquote(raw)
		if err != nil {
			continue
		}
		imports[unquoted] = true
	}
	return imports, true
}

// checkImportGraph is the gate check function registered in gateCheckRegistry.
// It returns (true, output) when at least one changed file introduces a new
// import edge that violates the layering rules.
func checkImportGraph(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	changed := changedNonTestGoFiles(ctx, workspace, run)
	if len(changed) == 0 {
		return false, ""
	}

	var findings []string

	for _, path := range changed {
		// Read the WORKING TREE (disk) version of the file. The gate runs
		// before the executor commits, so the index is STALE (== HEAD). Reading
		// from disk is the only way to see the coder's edits.
		diskBytes, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil {
			// Cannot read disk copy; skip this file (fail-open).
			continue
		}
		diskImports, ok := parseImports(path, string(diskBytes))
		if !ok {
			// Unparseable WIP; build check will catch it; skip here.
			continue
		}

		// Read the HEAD version of the file to determine pre-existing imports.
		// An error means the file is new; treat HEAD imports as empty.
		headSrc, headErr := run(ctx, workspace, nil, "git", "show", "HEAD:"+path)
		var headImports map[string]bool
		if headErr == nil {
			// Best-effort parse of HEAD; a parse error yields an empty set,
			// which is conservative (all current imports look "new").
			headImports, _ = parseImports(path, headSrc)
		}

		// Determine the module-relative importer path from the file's directory.
		// E.g. "pkg/cli/cache.go" -> dir "pkg/cli" -> importer "pkg/cli"
		importerDir := filepath.ToSlash(filepath.Dir(path))
		importerModRel := importerDir // module-relative (no module prefix)

		// Evaluate layering rules only for new imports within this module.
		for imp := range diskImports {
			if headImports[imp] {
				continue // pre-existing edge; not a new violation
			}
			if !strings.HasPrefix(imp, llmkubeModule+"/") {
				continue // external or stdlib import; never flagged
			}
			// Strip the module prefix to get the module-relative imported path.
			importedModRel := strings.TrimPrefix(imp, llmkubeModule+"/")

			for _, rule := range layerRules {
				if strings.HasPrefix(importerModRel, rule.fromPrefix) &&
					strings.HasPrefix(importedModRel, rule.toPrefix) {
					findings = append(findings, fmt.Sprintf(
						"%s: new import %s -> %s violates layering (%s)",
						path, importerModRel, importedModRel, rule.reason,
					))
				}
			}
		}
	}

	if len(findings) == 0 {
		return false, ""
	}
	return true, strings.Join(findings, "\n") + "\n"
}

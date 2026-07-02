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

// checkRBACUse performs a static RBAC-use analysis: it parses controller Go
// files with go/ast, extracts client verb calls (Create, Get, List, Update,
// Patch, Delete, Watch), maps the object type to a (group, resource) tuple via
// a static table, and verifies that a matching +kubebuilder:rbac marker exists
// somewhere in the same package.
//
// Design decisions:
//   - Syntactic AST only (go/parser, not go/types). Full type-loading is too
//     expensive for an in-loop gate check; the static table covers the types the
//     operator actually uses.
//   - Unknown types are SKIPPED, never flagged. This guarantees no false positives
//     for dependencies we have not enumerated.
//   - The check is fail-open: any parse error causes it to return (false, "").
//   - Only files under internal/controller/ or internal/foreman/controller/ are
//     inspected. Other directories are not expected to hold +kubebuilder:rbac
//     markers and are not judged.
package agent

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// rbacGroupResource maps a bare Go type name to the Kubernetes (group, resource)
// pair used in +kubebuilder:rbac markers. The key is the local type identifier
// (without a package qualifier). Unknown types are silently skipped so there are
// no false positives from types we have not enumerated.
var rbacGroupResource = map[string][2]string{
	"Job":                   {"batch", "jobs"},
	"Pod":                   {"", "pods"},
	"PersistentVolumeClaim": {"", "persistentvolumeclaims"},
	"Service":               {"", "services"},
	"ConfigMap":             {"", "configmaps"},
	"Secret":                {"", "secrets"},
	"Deployment":            {"apps", "deployments"},
	"InferenceService":      {"inference.llmkube.dev", "inferenceservices"},
	"Model":                 {"inference.llmkube.dev", "models"},
}

// rbacClientVerbs are the sigs.k8s.io/controller-runtime client verbs we track.
var rbacClientVerbs = map[string]string{
	"Create": "create",
	"Get":    "get",
	"List":   "list",
	"Update": "update",
	"Patch":  "patch",
	"Delete": "delete",
	"Watch":  "watch",
}

// controllerDirs are the workspace-relative prefixes where controller code lives.
var controllerDirs = []string{
	"internal/controller/",
	"internal/foreman/controller/",
}

// checkRBACUse is the gate check function registered in gateCheckRegistry.
// It returns (true, output) when a changed controller file uses a client verb
// on a known type without a matching +kubebuilder:rbac marker in the package.
func checkRBACUse(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	// Collect changed non-test Go files that live in a controller directory.
	all := changedNonTestGoFiles(ctx, workspace, run)
	var controllerFiles []string
	for _, f := range all {
		for _, pfx := range controllerDirs {
			if strings.HasPrefix(f, pfx) {
				controllerFiles = append(controllerFiles, f)
				break
			}
		}
	}
	if len(controllerFiles) == 0 {
		return false, ""
	}

	// Group controller files by their package directory (workspace-relative).
	byDir := map[string][]string{}
	for _, f := range controllerFiles {
		d := filepath.ToSlash(filepath.Dir(f))
		byDir[d] = append(byDir[d], f)
	}

	var findings []string

	for dir, changedInDir := range byDir {
		absDir := filepath.Join(workspace, filepath.FromSlash(dir))

		// List all .go files in the directory (no build-tag filtering needed;
		// we are doing a syntactic, fail-open scan). os.ReadDir replaces the
		// deprecated parser.ParseDir API.
		dirEntries, err := os.ReadDir(absDir)
		if err != nil {
			// Fail-open: skip this directory rather than blocking the coder.
			continue
		}

		fset := token.NewFileSet()
		var allFiles []*ast.File
		for _, de := range dirEntries {
			name := de.Name()
			if de.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, parseErr := parser.ParseFile(fset, filepath.Join(absDir, name), nil, parser.ParseComments)
			if parseErr != nil {
				continue // fail-open on individual parse errors
			}
			allFiles = append(allFiles, f)
		}

		// Collect all +kubebuilder:rbac markers from ALL files in the package
		// (markers may live in a sibling file, e.g. <kind>_controller.go).
		markers := collectRBACMarkers(allFiles)

		// Build a set of changed file base-names for quick lookup.
		changedBase := map[string]bool{}
		for _, f := range changedInDir {
			changedBase[filepath.Base(f)] = true
		}

		// Walk only the changed files' ASTs for client verb calls.
		for _, file := range allFiles {
			filename := fset.File(file.Pos()).Name()
			if !changedBase[filepath.Base(filename)] {
				continue
			}
			// Walk the AST for CallExpr nodes.
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				verb, typeName, ok := extractClientCall(call)
				if !ok {
					return true
				}
				// Strip trailing "List" for List/Watch calls.
				lookupName := typeName
				if (verb == "list" || verb == "watch") && strings.HasSuffix(lookupName, "List") {
					lookupName = strings.TrimSuffix(lookupName, "List")
				}
				gr, known := rbacGroupResource[lookupName]
				if !known {
					return true // unknown type: skip, no false positive
				}
				group, resource := gr[0], gr[1]
				if !markerCovers(markers, group, resource, verb) {
					relFile := filepath.Join(dir, filepath.Base(filename))
					// Choose friendly display of the group for the error message.
					displayGroup := group
					if displayGroup == "" {
						displayGroup = "core"
					}
					findings = append(findings, fmt.Sprintf(
						"%s: r.%s(&%s{}) needs a marker: // +kubebuilder:rbac:groups=%s,resources=%s,verbs=%s",
						relFile, capitalizeFirst(verb), typeName, displayGroup, resource, verb,
					))
				}
				return true
			})
		}
	}

	if len(findings) == 0 {
		return false, ""
	}
	return true, strings.Join(findings, "\n") + "\n"
}

// extractClientCall inspects a *ast.CallExpr and returns (verb, typeName, true)
// when it matches the pattern for a controller-runtime client call:
//
//	r.Create(ctx, &batchv1.Job{...})      -> "create", "Job", true
//	r.Client.List(ctx, &JobList{})        -> "list", "JobList", true
//	r.Status().Update(ctx, &Pod{...})     -> "update", "Pod", true
//
// Returns ("", "", false) for any other expression.
func extractClientCall(call *ast.CallExpr) (verb, typeName string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	methodName := sel.Sel.Name
	verb, isClientVerb := rbacClientVerbs[methodName]
	if !isClientVerb {
		return "", "", false
	}
	if !isClientReceiver(sel.X) {
		return "", "", false
	}

	// Extract the object argument. controller-runtime signatures differ by verb:
	//   Create/Update/Patch/Delete/List/Watch: (ctx, obj, opts...) -> object at index 1
	//   Get:                                   (ctx, key, obj, opts...) -> object at index 2
	var objArg ast.Expr
	if verb == "get" {
		// Get requires at least 3 args: ctx, key, obj.
		if len(call.Args) < 3 {
			return "", "", false
		}
		objArg = call.Args[2]
	} else {
		if len(call.Args) < 2 {
			return "", "", false
		}
		objArg = call.Args[1]
	}

	typeName, ok = extractObjectTypeName(objArg)
	if !ok {
		return "", "", false
	}
	return verb, typeName, true
}

// isClientReceiver reports whether expr looks like a receiver expression for
// a controller-runtime client call: r, r.Client, or r.Status() / r.Status(ctx).
func isClientReceiver(expr ast.Expr) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		// Bare receiver like `r`
		return true
	case *ast.SelectorExpr:
		// r.Client, r.SomeField, etc. The loose ident.field match is intentional
		// and safe: unknown object types are not in rbacGroupResource and are
		// silently skipped (fail-open), so no false positives arise.
		_, lhsIsIdent := x.X.(*ast.Ident)
		return lhsIsIdent
	case *ast.CallExpr:
		// r.Status(), r.Status(ctx) -- Status().Update(...)
		innerSel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		_, lhsIsIdent := innerSel.X.(*ast.Ident)
		return lhsIsIdent
	}
	return false
}

// extractObjectTypeName extracts the bare type name from an argument expression.
// Handles:
//
//	&batchv1.Job{}   (*ast.UnaryExpr -> *ast.CompositeLit with SelectorExpr type)
//	&Job{}           (*ast.UnaryExpr -> *ast.CompositeLit with Ident type)
//	batchv1.Job{}    (*ast.CompositeLit with SelectorExpr type)
//	Job{}            (*ast.CompositeLit with Ident type)
func extractObjectTypeName(expr ast.Expr) (string, bool) {
	// Unwrap & operator.
	if unary, ok := expr.(*ast.UnaryExpr); ok {
		expr = unary.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	return typeIdentName(lit.Type)
}

// typeIdentName returns the bare type name from a type expression, stripping
// any package qualifier. Returns ("", false) for non-name expressions.
func typeIdentName(typeExpr ast.Expr) (string, bool) {
	switch t := typeExpr.(type) {
	case *ast.Ident:
		return t.Name, true
	case *ast.SelectorExpr:
		return t.Sel.Name, true
	}
	return "", false
}

// rbacMarker represents a parsed +kubebuilder:rbac comment.
type rbacMarker struct {
	groups    []string
	resources []string
	verbs     []string
}

// collectRBACMarkers scans every comment group in the provided files and
// returns the parsed markers.
func collectRBACMarkers(files []*ast.File) []rbacMarker {
	var markers []rbacMarker
	for _, file := range files {
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				text := strings.TrimPrefix(c.Text, "//")
				text = strings.TrimSpace(text)
				if !strings.HasPrefix(text, "+kubebuilder:rbac:") {
					continue
				}
				if m, ok := parseRBACMarker(text); ok {
					markers = append(markers, m)
				}
			}
		}
	}
	return markers
}

// parseRBACMarker parses a "+kubebuilder:rbac:groups=...,resources=...,verbs=..."
// string into an rbacMarker. Returns (zero, false) on parse errors.
func parseRBACMarker(text string) (rbacMarker, bool) {
	// Strip prefix.
	rest := strings.TrimPrefix(text, "+kubebuilder:rbac:")
	if rest == text {
		return rbacMarker{}, false
	}

	// Parse key=value pairs. The commas separate key=value pairs at the TOP
	// level, but verbs values may themselves use commas (or semicolons) as
	// separators. We handle this by splitting on top-level commas that are
	// followed by a known key name.
	//
	// Simplest correct approach: split on "," and re-join tokens that do not
	// look like "key=..." onto the previous token. Known keys: groups, resources,
	// verbs, namespace, urls.
	kvMap := splitMarkerKVs(rest)

	groups := splitListValue(kvMap["groups"])
	resources := splitListValue(kvMap["resources"])
	verbsRaw := kvMap["verbs"]
	// verbs can be semicolon- or comma-separated; splitListValue already handles both.
	verbs := splitListValue(verbsRaw)

	if len(groups) == 0 || len(resources) == 0 || len(verbs) == 0 {
		return rbacMarker{}, false
	}
	return rbacMarker{groups: groups, resources: resources, verbs: verbs}, true
}

// splitMarkerKVs splits a marker body (everything after "+kubebuilder:rbac:")
// into a map of key -> raw value. It handles the tricky case where a value
// itself contains commas (e.g. verbs=get,list,create) by recognising that a
// new key always starts a token immediately after a comma.
func splitMarkerKVs(body string) map[string]string {
	knownKeys := map[string]bool{
		"groups": true, "resources": true, "verbs": true,
		"namespace": true, "urls": true, "resourceNames": true,
	}
	result := map[string]string{}

	// Split on commas, then check if the token starts with a known key= prefix.
	// If yes, start a new KV; otherwise append to the previous value.
	parts := strings.Split(body, ",")
	var currentKey, currentVal string
	for _, part := range parts {
		eqIdx := strings.IndexByte(part, '=')
		if eqIdx >= 0 {
			possibleKey := part[:eqIdx]
			if knownKeys[possibleKey] {
				// Save previous KV.
				if currentKey != "" {
					result[currentKey] = currentVal
				}
				currentKey = possibleKey
				currentVal = part[eqIdx+1:]
				continue
			}
		}
		// Not a new key: append this token to the current value with a comma.
		if currentKey != "" {
			currentVal += "," + part
		}
	}
	if currentKey != "" {
		result[currentKey] = currentVal
	}
	return result
}

// splitListValue splits a marker list value on both commas and semicolons and
// returns trimmed, non-empty entries. It handles the two common serializations:
//   - groups=batch,apps   (comma-separated, same as key-level commas but safe
//     because values are parsed after key extraction)
//   - verbs=get;list;create  (semicolon-separated)
func splitListValue(v string) []string {
	// Normalise semicolons to commas then split.
	v = strings.ReplaceAll(v, ";", ",")
	var out []string
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		// Strip surrounding double-quotes before the emptiness check. This
		// handles the kubebuilder convention of writing the core API group as
		// groups="" (quoted empty string). After stripping, a token that was
		// originally "" becomes the empty string, which is the correct group
		// value for the Kubernetes core API group. We only skip tokens that
		// were empty BEFORE stripping (i.e. they were whitespace-only or
		// produced by a trailing comma), distinguished here by checking whether
		// the pre-strip token was non-empty.
		stripped := strings.Trim(tok, "\"")
		if tok == "" {
			continue // skip genuinely-empty / whitespace-only tokens
		}
		out = append(out, stripped)
	}
	return out
}

// normalizeGroup canonicalises the Kubernetes API group so that "core" and ""
// are treated as identical. kubebuilder markers accept both spellings for the
// core group (groups="" and groups=core); the static type table uses "".
func normalizeGroup(g string) string {
	if g == "core" {
		return ""
	}
	return g
}

// markerCovers reports whether any of the collected markers grants the
// combination (group, resource, verb). A marker covers when its groups list
// contains group (or "*"), its resources list contains resource (or "*"), and
// its verbs list contains verb (or "*").
//
// Both the used group and each marker group are passed through normalizeGroup
// so that "core" and "" are treated as equivalent.
func markerCovers(markers []rbacMarker, group, resource, verb string) bool {
	normGroup := normalizeGroup(group)
	for _, m := range markers {
		groupMatch := false
		for _, mg := range m.groups {
			if normalizeGroup(mg) == normGroup || mg == "*" {
				groupMatch = true
				break
			}
		}
		if groupMatch &&
			listContains(m.resources, resource) &&
			listContains(m.verbs, verb) {
			return true
		}
	}
	return false
}

// listContains reports whether needle is in haystack or haystack contains "*".
func listContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle || s == "*" {
			return true
		}
	}
	return false
}

// capitalizeFirst returns s with its first byte upper-cased.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

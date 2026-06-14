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

package tools

import (
	"reflect"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/tools/catalog"
)

// TestCatalogNamesMatchConstructors is the drift guard for the leaf
// catalog package. The Agent admission webhook validates spec.tools
// against catalog.CanonicalToolNames (a dependency-free literal list) so
// the operator binary stays free of the executor's dependency tree. This
// test lives in the real tools package (which DOES import the executor
// types) and asserts the literal list equals the names the actual tool
// constructors return. If someone adds, removes, or renames a tool, this
// fails until the literal catalog is updated.
func TestCatalogNamesMatchConstructors(t *testing.T) {
	r, err := New(canonicalToolSet()...)
	if err != nil {
		t.Fatalf("build canonical registry: %v", err)
	}
	fromConstructors := r.Names() // sorted
	fromCatalog := catalog.CanonicalToolNames()

	if !reflect.DeepEqual(fromConstructors, fromCatalog) {
		t.Fatalf("catalog drift: leaf catalog.CanonicalToolNames() = %v, but tool "+
			"constructors yield %v; update pkg/foreman/agent/tools/catalog/catalog.go",
			fromCatalog, fromConstructors)
	}
}

// canonicalToolSet constructs one of every tool the v0.1 surface exposes,
// mirroring makeRegistryFactory in cmd/foreman-agent. The constructors
// take a zero workspace + nil collaborators on purpose: this set exists
// only to read Name() for the drift check above, never to Dispatch().
func canonicalToolSet() []Tool {
	return []Tool{
		&ReadFileTool{},
		&WriteFileTool{},
		&StrReplaceTool{},
		&GrepTool{},
		&BashTool{},
		SubmitResultTool{},
		&RunGateJobTool{},
		&FetchIssueTool{},
	}
}

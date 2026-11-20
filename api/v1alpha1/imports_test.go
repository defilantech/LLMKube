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

package v1alpha1

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAPIImportsNoRepoPackages pins api/ as a leaf: images such as
// Dockerfile.router-proxy copy only api/, cmd/ and internal/, so an import of
// another module package from api/ breaks their build.
func TestAPIImportsNoRepoPackages(t *testing.T) {
	const module = "github.com/defilantech/llmkube/"
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, module) && !strings.HasPrefix(p, module+"api/") {
				t.Errorf("%s imports %s; api/ may import only api/ and third-party packages", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

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
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMain clears the proxy environment so the SSRF guard tests are
// hermetic: a developer's HTTP_PROXY would otherwise send test hostnames
// such as mirror.test to a real proxy. Tests that exercise proxy behavior
// set these variables themselves with t.Setenv.
//
// It also clears systemTempRoots: many tests build a model store in
// t.TempDir(), which is under /tmp wherever TMPDIR is unset (Linux CI), and
// the store check would refuse all of them. The /tmp refusal tests set it
// back explicitly with withSystemTempRoots.
//
// It also points the oMLX base path at a temp dir, so no oMLX executor a
// test builds can write the developer's real ~/.omlx/model_settings.json.
func TestMain(m *testing.M) {
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "REQUEST_METHOD"} {
		_ = os.Unsetenv(k)
	}
	systemTempRoots = nil
	omlxBase, err := os.MkdirTemp("", "omlx-base-")
	if err != nil {
		panic(err)
	}
	omlxDefaultBasePath = func() string { return omlxBase }
	code := m.Run()
	_ = os.RemoveAll(omlxBase)
	os.Exit(code)
}

// productionSystemTempRoots captures the production default before TestMain
// clears the package variable, so a test can assert what ships.
var productionSystemTempRoots = append([]string(nil), systemTempRoots...)

// withSystemTempRoots restores the production systemTempRoots for one test.
func withSystemTempRoots(t *testing.T) {
	t.Helper()
	prev := systemTempRoots
	systemTempRoots = productionSystemTempRoots
	t.Cleanup(func() { systemTempRoots = prev })
}

// The production default refuses every shared temporary directory, not just
// /tmp: checkStoreNotInTmp is judged on both spellings of each root.
func TestCheckStoreNotInTmp_RefusesAllProductionRoots(t *testing.T) {
	want := []string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"}
	for _, root := range want {
		if !slices.Contains(productionSystemTempRoots, root) {
			t.Errorf("production systemTempRoots = %v, missing %s", productionSystemTempRoots, root)
		}
	}
	withSystemTempRoots(t)
	for _, root := range want {
		where := filepath.Join(root, "llmkube-models")
		if err := checkStoreNotInTmp(where, where, where); err == nil {
			t.Errorf("checkStoreNotInTmp(%s) = nil, want a refusal", where)
		}
	}
}

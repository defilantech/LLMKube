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

package controller

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Goldens of the generated init command for Models WITHOUT spec.sha256.
// Fleet rollout (#1965) promises that enabling the operator's verify machinery
// does not churn the pod templates of every workload that did not opt in, and
// that their transfers keep behaving exactly as before. A token-absence check
// pins only the helpers; these goldens pin every byte of every branch, so a
// drive-by edit to an unhashed branch fails here even when it never touches
// the verify code. Regenerate with -update only when a change to the unhashed
// command shape is intended and reviewed.

var updateInitCmdGoldens = flag.Bool("update-init-goldens", false, "rewrite the init-command golden files")

func TestModelInitCommand_UnhashedBranches_Golden(t *testing.T) {
	kinds := []struct {
		name     string
		isLocal  bool
		isS3     bool
		isHFAuth bool
	}{
		{"http", false, false, false},
		{"s3", false, true, false},
		{"local", true, false, false},
		{"hf", false, false, true},
	}
	for _, useCache := range []bool{true, false} {
		for _, kind := range kinds {
			for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
				name := fmt.Sprintf("initcmd_usecache-%v_%s_%s.golden", useCache, kind.name, policy)
				cmd := buildModelInitCommand(kind.isLocal, kind.isS3, useCache, kind.isHFAuth, false, policy)
				path := filepath.Join("testdata", name)
				if *updateInitCmdGoldens {
					if err := os.MkdirAll("testdata", 0o755); err != nil {
						t.Fatalf("mkdir testdata: %v", err)
					}
					if err := os.WriteFile(path, []byte(cmd), 0o644); err != nil {
						t.Fatalf("write golden %s: %v", name, err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden %s (generate with -update-init-goldens): %v", name, err)
				}
				if cmd != string(want) {
					t.Errorf("unhashed command for %s drifted from the golden:\ngot:\n%s\nwanted:\n%s", name, cmd, want)
				}
				for _, tok := range sha256HelperTokens {
					if strings.Contains(cmd, tok) {
						t.Errorf("golden %s contains verify helper %q", name, tok)
					}
				}
			}
		}
	}
}

// Goldens of the generated multi-file init command for Models WITHOUT
// per-file digests. Captured from the pre-#1978 builder, so they prove the
// withSHA256=false branch is byte-for-byte what it was before per-file verify
// gates were added. Regenerate with -update-init-goldens only when a change to
// the unhashed multi-file command shape is intended and reviewed.
func TestModelMultiFileInitCommand_UnhashedBranches_Golden(t *testing.T) {
	kinds := []struct {
		name     string
		isS3     bool
		isHFAuth bool
	}{
		{"http", false, false},
		{"s3", true, false},
		{"hf", false, true},
	}
	for _, useCache := range []bool{true, false} {
		for _, kind := range kinds {
			for _, policy := range []string{RefreshPolicyIfNotPresent, RefreshPolicyOnChange} {
				name := fmt.Sprintf("initcmd_multi_usecache-%v_%s_%s.golden", useCache, kind.name, policy)
				cmd := buildMultiFileInitCommand(useCache, kind.isS3, kind.isHFAuth, false, policy)
				path := filepath.Join("testdata", name)
				if *updateInitCmdGoldens {
					if err := os.MkdirAll("testdata", 0o755); err != nil {
						t.Fatalf("mkdir testdata: %v", err)
					}
					if err := os.WriteFile(path, []byte(cmd), 0o644); err != nil {
						t.Fatalf("write golden %s: %v", name, err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read golden %s (generate with -update-init-goldens): %v", name, err)
				}
				if cmd != string(want) {
					t.Errorf("unhashed multi-file command for %s drifted from the golden:\ngot:\n%s\nwanted:\n%s", name, cmd, want)
				}
				for _, tok := range sha256HelperTokens {
					if strings.Contains(cmd, tok) {
						t.Errorf("golden %s contains verify helper %q", name, tok)
					}
				}
			}
		}
	}
}

// TestModelInitCommand_SHA256Branches_HasGates keeps the positive half of the
// byte-identity pin: the disabled goldens are only meaningful if the enabled
// path demonstrably wires the gates in.
func TestModelInitCommand_SHA256Branches_HasGates(t *testing.T) {
	if cmd := buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent); !strings.Contains(cmd, "llmkube_publish_sha256") || !strings.Contains(cmd, "llmkube_check_sha256") {
		t.Errorf("sha256-enabled IfNotPresent command is missing the publish/check gates")
	}
	// The OnChange guard is hoisted to the head of the composed command (it
	// must run before the debris sweep), so assert on the builder output.
	cmd := buildModelInitCommand(false, false, true, false, true, RefreshPolicyOnChange)
	for _, tok := range []string{"llmkube_precheck_sha256", "llmkube_check_sha256", "llmkube_publish_sha256"} {
		if !strings.Contains(cmd, tok) {
			t.Errorf("sha256-enabled OnChange command is missing the %s gate", tok)
		}
	}
}

// TestModelInitCommand_SHA256GuardRunsFirst pins that the known-rejected
// guard runs before anything that could clear its evidence: every
// sha256-enabled command issues llmkube_precheck_sha256 before the first
// sweep, probe or transfer statement.
func TestModelInitCommand_SHA256GuardRunsFirst(t *testing.T) {
	cases := map[string]string{
		"cached local":    buildModelInitCommand(true, false, true, false, true, RefreshPolicyIfNotPresent),
		"cached s3":       buildModelInitCommand(false, true, true, false, true, RefreshPolicyIfNotPresent),
		"cached http":     buildModelInitCommand(false, false, true, false, true, RefreshPolicyIfNotPresent),
		"cached onchange": buildModelInitCommand(false, false, true, false, true, RefreshPolicyOnChange),
		"uncached s3":     buildModelInitCommand(false, true, false, false, true, RefreshPolicyIfNotPresent),
		"uncached http":   buildModelInitCommand(false, false, false, false, true, RefreshPolicyIfNotPresent),
	}
	for name, cmd := range cases {
		guard := `llmkube_precheck_sha256 || exit 1`
		gi := strings.Index(cmd, guard)
		if gi < 0 {
			t.Errorf("%s: the precheck guard is missing", name)
			continue
		}
		for _, late := range []string{`rm -f "$MODEL_PATH.tmp"`, `find "$(dirname`, `cp /host-model/model.gguf`, `download_with_progress "$MODEL_PATH.tmp"`} {
			if li := strings.Index(cmd, late); li >= 0 && li < gi {
				t.Errorf("%s: %q is emitted before the guard", name, late)
			}
		}
	}
}

// TestModelMultiFileInitCommand_OnChangeKeepCachedIsGated pins that the
// unreachable-origin fallback on both gated OnChange branches (s3 and
// http/hf) keeps a cached copy only through the per-file gate, so a pinned
// file that fails its digest cannot be served from a stale cache (#1978).
func TestModelMultiFileInitCommand_OnChangeKeepCachedIsGated(t *testing.T) {
	gated := `elif [ -f "$dest" ] && { if [ -n "${MODEL_SHA256:-}" ]; then llmkube_accept_file "$dest"; else llmkube_cached_size_matches "$remote_size"; fi; }; then if llmkube_marker_hit_sha256 "$dest"; then echo "Model artifact $rel: the origin served bytes that do not match the pinned spec.fileSha256; kept the pinned cached copy"`
	for _, kind := range []struct {
		name string
		isS3 bool
	}{
		{"http", false},
		{"s3", true},
	} {
		cmd := buildMultiFileInitCommand(true, kind.isS3, false, true, RefreshPolicyOnChange)
		if !strings.Contains(cmd, gated) {
			t.Errorf("%s: gated OnChange keep-cached fallback is not gated by llmkube_accept_file:\n%s", kind.name, cmd)
		}
		unhashed := buildMultiFileInitCommand(true, kind.isS3, false, false, RefreshPolicyOnChange)
		if strings.Contains(unhashed, "llmkube_accept_file") {
			t.Errorf("%s: unhashed OnChange command contains a verify helper", kind.name)
		}
	}
}

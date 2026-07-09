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
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDiffHasCodeLine(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want bool
	}{
		{"comment-only additions", "@@ -1,0 +2,2 @@\n+// a new comment\n+//   more comment\n", false},
		{"blank additions", "@@ -1,0 +2,1 @@\n+   \n", false},
		{"block comment lines", "@@ -1,0 +2,3 @@\n+/* doc\n+ * lines\n+ */\n", false},
		{"real code addition", "@@ -1,0 +2,1 @@\n+x := doWork()\n", true},
		{"real code removal", "@@ -2,1 +1,0 @@\n-return err\n", true},
		{"headers ignored, only comment body", "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n+// tweak\n", false},
		{"code among comments", "@@ -1,0 +2,2 @@\n+// note\n+cfg.Enabled = true\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffHasCodeLine(tc.diff); got != tc.want {
				t.Errorf("diffHasCodeLine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFunctionalChangeInDiff(t *testing.T) {
	// runner dispatches on the git subcommand: --name-only returns the file
	// list; a per-file `diff -U0 ... -- <f>` returns that file's diff body.
	runner := func(names string, perFile map[string]string, nameErr error) commandRunner {
		return func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
			if argsContain(args, "--name-only") {
				return names, nameErr
			}
			// last arg is the file path (after the "--" separator).
			f := args[len(args)-1]
			return perFile[f], nil
		}
	}

	tests := []struct {
		name    string
		names   string
		perFile map[string]string
		nameErr error
		want    bool
	}{
		{
			name:  "docs only",
			names: "docs/model-storage.md\nREADME.md",
			want:  false,
		},
		{
			name:  "tests only",
			names: "pkg/foreman/agent/thing_test.go",
			want:  false,
		},
		{
			name:    "comment-only production go",
			names:   "internal/controller/model_storage.go",
			perFile: map[string]string{"internal/controller/model_storage.go": "@@ -55,0 +56,3 @@\n+// On a tainted node...\n+// pre-stage the GGUF...\n"},
			want:    false,
		},
		{
			name:    "real production go change",
			names:   "internal/controller/model_storage.go",
			perFile: map[string]string{"internal/controller/model_storage.go": "@@ -55,0 +56,1 @@\n+recorder.Eventf(obj, \"Warning\", \"Tainted\", msg)\n"},
			want:    true,
		},
		{
			name:    "docs plus comment-only go",
			names:   "docs/x.md\ninternal/controller/model_storage.go",
			perFile: map[string]string{"internal/controller/model_storage.go": "@@ -1,0 +2,1 @@\n+// just a comment\n"},
			want:    false,
		},
		{
			name:    "git error degrades open (assume functional)",
			names:   "",
			nameErr: errors.New("boom"),
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := runner(tc.names, tc.perFile, tc.nameErr)
			if got := functionalChangeInDiff(context.Background(), "/ws", "main", run); got != tc.want {
				t.Errorf("functionalChangeInDiff = %v, want %v", got, tc.want)
			}
		})
	}
}

func argsContain(ss []string, want string) bool {
	for _, s := range ss {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

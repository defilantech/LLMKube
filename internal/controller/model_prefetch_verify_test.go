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

package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrefetchVerifyCommand_MissingOrUnreadable runs the model-verify script a
// prefetched pod runs: a staged file the pod's user cannot reach is reported as
// unreadable, with its uid and groups and the blocking path, not as missing,
// because a re-stage cannot fix an ownership mismatch (#2450).
func TestPrefetchVerifyCommand_MissingOrUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through any mode; the permission cases need a non-root user")
	}
	setup := func(t *testing.T) (root, cacheDir string) {
		t.Helper()
		root = filepath.Join(t.TempDir(), "models")
		cacheDir = filepath.Join(root, "k")
		if err := os.MkdirAll(filepath.Join(cacheDir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"config.json", "a.safetensors", "sub/b.safetensors"} {
			if err := os.WriteFile(filepath.Join(cacheDir, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// Restore owner access so the test's temp dir can be removed; a
		// directory needs its execute bit for that.
		t.Cleanup(func() {
			_ = os.Chmod(root, 0o700)                                     //nolint:gosec // the test's own temp dir, restored for removal
			_ = os.Chmod(cacheDir, 0o700)                                 //nolint:gosec // the test's own temp dir, restored for removal
			_ = os.Chmod(filepath.Join(cacheDir, "sub"), 0o700)           //nolint:gosec // the test's own temp dir, restored for removal
			_ = os.Chmod(filepath.Join(cacheDir, "a.safetensors"), 0o600) // owner read-write
		})
		return root, cacheDir
	}
	run := func(t *testing.T, cacheDir string, files ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", buildPrefetchVerifyCommand(false))
		cmd.Env = append(os.Environ(), "CACHE_DIR="+cacheDir, "MODEL_FILES="+strings.Join(files, "\n"), "MODEL_NAME=qwen")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// mode is the blocking path's mode as ls -l prints it; the message lists
	// that path last, so "<path>. Model" pins which path was listed.
	unreadable := func(t *testing.T, out string, err error, blocked, mode string) {
		t.Helper()
		if err == nil {
			t.Fatalf("verify passed on a cache it cannot read:\n%s", out)
		}
		if !strings.Contains(out, "ERROR: PrefetchedFilesUnreadable") {
			t.Fatalf("not reported as unreadable:\n%s", out)
		}
		if strings.Contains(out, "PrefetchedFilesMissing") {
			t.Errorf("an unreadable file also reported as missing, which would trigger a useless re-stage:\n%s", out)
		}
		if !strings.Contains(out, "uid ") || !strings.Contains(out, "blocked at: "+mode) || !strings.Contains(out, blocked+". Model") {
			t.Errorf("the message should name the uid and list the blocking path %q with mode %s:\n%s", blocked, mode, out)
		}
	}

	t.Run("every staged file is readable", func(t *testing.T) {
		_, cacheDir := setup(t)
		if out, err := run(t, cacheDir, "config.json", "a.safetensors"); err != nil {
			t.Fatalf("verify failed on a readable cache: %v\n%s", err, out)
		}
	})

	t.Run("a staged file is absent", func(t *testing.T) {
		_, cacheDir := setup(t)
		out, err := run(t, cacheDir, "config.json", "b.safetensors")
		if err == nil || !strings.Contains(out, "ERROR: PrefetchedFilesMissing") {
			t.Fatalf("an absent file not reported as missing: %v\n%s", err, out)
		}
		if strings.Contains(out, "PrefetchedFilesUnreadable") {
			t.Errorf("an absent file reported as unreadable:\n%s", out)
		}
	})

	t.Run("the cache root cannot be entered", func(t *testing.T) {
		root, cacheDir := setup(t)
		if err := os.Chmod(root, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, cacheDir, "config.json")
		unreadable(t, out, err, root, "drw-------")
	})

	t.Run("the cache directory cannot be entered", func(t *testing.T) {
		_, cacheDir := setup(t)
		if err := os.Chmod(cacheDir, 0o400); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, cacheDir, "config.json")
		unreadable(t, out, err, cacheDir, "dr--------")
	})

	t.Run("a staged file cannot be read", func(t *testing.T) {
		_, cacheDir := setup(t)
		f := filepath.Join(cacheDir, "a.safetensors")
		if err := os.Chmod(f, 0o200); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, cacheDir, "config.json", "a.safetensors")
		unreadable(t, out, err, f, "--w-------")
	})

	t.Run("a directory below the cache directory cannot be entered", func(t *testing.T) {
		_, cacheDir := setup(t)
		sub := filepath.Join(cacheDir, "sub")
		if err := os.Chmod(sub, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, cacheDir, "config.json", "sub/b.safetensors")
		unreadable(t, out, err, sub, "drw-------")
	})
}

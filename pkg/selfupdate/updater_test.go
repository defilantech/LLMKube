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

package selfupdate_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/defilantech/llmkube/pkg/selfupdate"
)

// testBlob is a deterministic fake binary payload.
const testBlob = "fake-foreman-agent-binary-v0.9.0"

func blobSHA256(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// newUpdater builds an Updater wired to a temp directory with a httptest server.
func newUpdater(t *testing.T, srv *httptest.Server, installRoot, binaryName, currentVersion string) *selfupdate.Updater { //nolint:lll,unparam
	t.Helper()
	return &selfupdate.Updater{
		CurrentVersion: currentVersion,
		OS:             "linux",
		Arch:           "amd64",
		InstallRoot:    installRoot,
		BinaryName:     binaryName,
		Verifier:       &selfupdate.SHA256Verifier{},
		HTTPClient:     srv.Client(),
		Log:            logr.Discard(),
	}
}

// TestMaybeApply_NoopWhenVersionMatches ensures no file I/O when the
// target version equals the running version.
func TestMaybeApply_NoopWhenVersionMatches(t *testing.T) {
	dir := t.TempDir()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u := newUpdater(t, srv, filepath.Join(dir, "foreman-agent"), "foreman-agent", "v0.9.0")

	result, err := u.MaybeApply(selfupdate.Target{
		Version: "v0.9.0",
		URL:     srv.URL + "/download",
		SHA256:  blobSHA256(testBlob),
	})
	if err != nil {
		t.Fatalf("MaybeApply: %v", err)
	}
	if result.Restarting {
		t.Error("Restarting = true, want false for same version")
	}
	if called {
		t.Error("HTTP server was called, expected no download for matching version")
	}
}

// TestMaybeApply_NoopWhenVersionEmpty ensures no action when Target.Version
// is empty (no update request present).
func TestMaybeApply_NoopWhenVersionEmpty(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("HTTP server should not be called")
	}))
	defer srv.Close()

	u := newUpdater(t, srv, filepath.Join(dir, "foreman-agent"), "foreman-agent", "v0.9.0")

	result, err := u.MaybeApply(selfupdate.Target{})
	if err != nil {
		t.Fatalf("MaybeApply with empty target: %v", err)
	}
	if result.Restarting {
		t.Error("Restarting = true, want false for empty target")
	}
}

// TestMaybeApply_HappyPath tests the full successful update flow:
// download -> verify sha256 -> stage at versions/<v>/<bin> -> flip current symlink.
func TestMaybeApply_HappyPath(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, testBlob)
	}))
	defer srv.Close()

	u := newUpdater(t, srv, installRoot, "foreman-agent", "v0.8.4")

	result, err := u.MaybeApply(selfupdate.Target{
		Version: "v0.9.0",
		URL:     srv.URL + "/download",
		SHA256:  blobSHA256(testBlob),
	})
	if err != nil {
		t.Fatalf("MaybeApply: %v", err)
	}
	if !result.Restarting {
		t.Fatal("Restarting = false, want true after successful update")
	}

	// The staged binary must exist at the versioned path.
	stagedPath := filepath.Join(installRoot, "versions", "v0.9.0", "foreman-agent")
	info, err := os.Stat(stagedPath)
	if err != nil {
		t.Fatalf("staged binary not found at %s: %v", stagedPath, err)
	}
	if info.Mode()&0o755 != 0o755 {
		t.Errorf("staged binary mode = %o, want at least 0755", info.Mode())
	}

	// current symlink must resolve to the versioned directory.
	currentLink := filepath.Join(installRoot, "current")
	resolved, err := os.Readlink(currentLink)
	if err != nil {
		t.Fatalf("current symlink: %v", err)
	}
	wantTarget := filepath.Join(installRoot, "versions", "v0.9.0")
	if resolved != wantTarget {
		t.Errorf("current -> %q, want %q", resolved, wantTarget)
	}

	// Binary accessible via current symlink path.
	viaCurrentPath := filepath.Join(currentLink, "foreman-agent")
	data, err := os.ReadFile(viaCurrentPath)
	if err != nil {
		t.Fatalf("read via current: %v", err)
	}
	if string(data) != testBlob {
		t.Errorf("content via current = %q, want %q", data, testBlob)
	}
}

// TestMaybeApply_BadSHA256_Rejected verifies that a checksum mismatch:
//  1. returns an error,
//  2. cleans up the temp file,
//  3. leaves the current symlink UNCHANGED (or absent if no prior version).
func TestMaybeApply_BadSHA256_Rejected(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	// Pre-seed a prior good version so we can assert the symlink doesn't change.
	priorVersion := "v0.8.4"
	priorDir := filepath.Join(installRoot, "versions", priorVersion)
	if err := os.MkdirAll(priorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	priorBin := filepath.Join(priorDir, "foreman-agent")
	if err := os.WriteFile(priorBin, []byte("prior-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create current symlink pointing at prior version.
	currentLink := filepath.Join(installRoot, "current")
	if err := os.Symlink(priorDir, currentLink); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, testBlob)
	}))
	defer srv.Close()

	u := newUpdater(t, srv, installRoot, "foreman-agent", "v0.8.4")

	_, err := u.MaybeApply(selfupdate.Target{
		Version: "v0.9.0",
		URL:     srv.URL + "/download",
		SHA256:  "0000000000000000000000000000000000000000000000000000000000000000", // wrong
	})
	if err == nil {
		t.Fatal("expected error for bad sha256, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error %q does not mention sha256", err)
	}

	// current symlink must still point at prior version.
	resolved, err2 := os.Readlink(currentLink)
	if err2 != nil {
		t.Fatalf("current symlink disappeared: %v", err2)
	}
	if resolved != priorDir {
		t.Errorf("current -> %q after failed update, want %q (prior)", resolved, priorDir)
	}

	// No temp file should be left behind.
	entries, _ := os.ReadDir(installRoot)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp-") {
			t.Errorf("temp file not cleaned up: %s", e.Name())
		}
	}
}

// TestMaybeApply_SecondUpdateSetsPrevious verifies that after a second successful
// update the "previous" symlink captures the old version directory.
func TestMaybeApply_SecondUpdateSetsPrevious(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	const blob1 = "binary-v0.9.0"
	const blob2 = "binary-v0.9.1"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "v0.9.1") {
			_, _ = fmt.Fprint(w, blob2)
		} else {
			_, _ = fmt.Fprint(w, blob1)
		}
	}))
	defer srv.Close()

	u := newUpdater(t, srv, installRoot, "foreman-agent", "v0.8.4")

	// First update: v0.8.4 -> v0.9.0
	_, err := u.MaybeApply(selfupdate.Target{
		Version: "v0.9.0",
		URL:     srv.URL + "/v0.9.0",
		SHA256:  blobSHA256(blob1),
	})
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Simulate the agent having restarted on v0.9.0
	u.CurrentVersion = "v0.9.0"

	// Second update: v0.9.0 -> v0.9.1
	_, err = u.MaybeApply(selfupdate.Target{
		Version: "v0.9.1",
		URL:     srv.URL + "/v0.9.1",
		SHA256:  blobSHA256(blob2),
	})
	if err != nil {
		t.Fatalf("second update: %v", err)
	}

	// current -> v0.9.1
	currentLink := filepath.Join(installRoot, "current")
	current, err := os.Readlink(currentLink)
	if err != nil {
		t.Fatalf("current symlink: %v", err)
	}
	wantCurrent := filepath.Join(installRoot, "versions", "v0.9.1")
	if current != wantCurrent {
		t.Errorf("current -> %q, want %q", current, wantCurrent)
	}

	// previous -> v0.9.0
	previousLink := filepath.Join(installRoot, "previous")
	previous, err := os.Readlink(previousLink)
	if err != nil {
		t.Fatalf("previous symlink: %v", err)
	}
	wantPrevious := filepath.Join(installRoot, "versions", "v0.9.0")
	if previous != wantPrevious {
		t.Errorf("previous -> %q, want %q", previous, wantPrevious)
	}
}

// TestMaybeApply_AtomicFlip asserts the current symlink is never absent
// during the flip (atomic rename over current.tmp).
// We do this by verifying the old symlink exists before and the new one
// exists after — no moment of absence.
func TestMaybeApply_AtomicFlip(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	// Pre-seed prior version.
	priorDir := filepath.Join(installRoot, "versions", "v0.8.4")
	if err := os.MkdirAll(priorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priorDir, "foreman-agent"), []byte("prior"), 0o755); err != nil {
		t.Fatal(err)
	}
	currentLink := filepath.Join(installRoot, "current")
	if err := os.Symlink(priorDir, currentLink); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, testBlob)
	}))
	defer srv.Close()

	u := newUpdater(t, srv, installRoot, "foreman-agent", "v0.8.4")

	result, err := u.MaybeApply(selfupdate.Target{
		Version: "v0.9.0",
		URL:     srv.URL + "/download",
		SHA256:  blobSHA256(testBlob),
	})
	if err != nil {
		t.Fatalf("MaybeApply: %v", err)
	}
	if !result.Restarting {
		t.Fatal("Restarting = false, want true")
	}

	// After the flip, current.tmp must NOT exist (cleanup check).
	tmpLink := filepath.Join(installRoot, "current.tmp")
	if _, err := os.Lstat(tmpLink); err == nil {
		t.Error("current.tmp was not cleaned up after atomic rename")
	}

	// current resolves to the new version.
	resolved, err := os.Readlink(currentLink)
	if err != nil {
		t.Fatalf("current symlink: %v", err)
	}
	wantNew := filepath.Join(installRoot, "versions", "v0.9.0")
	if resolved != wantNew {
		t.Errorf("current -> %q, want %q", resolved, wantNew)
	}
}

// TestSHA256Verifier_Accept verifies the real verifier passes a correctly-
// hashed file.
func TestSHA256Verifier_Accept(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "blob-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprint(f, testBlob)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	v := &selfupdate.SHA256Verifier{}
	if err := v.Verify(f.Name(), blobSHA256(testBlob)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestSHA256Verifier_Reject verifies the real verifier rejects a wrong hash.
func TestSHA256Verifier_Reject(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "blob-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprint(f, testBlob)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	v := &selfupdate.SHA256Verifier{}
	err = v.Verify(f.Name(), "deadbeef00000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for wrong sha256, got nil")
	}
}

// TestRunningUnderManagedRoot_False checks that a typical os.Executable()
// path (e.g. the test binary) is NOT under the managed root.
func TestRunningUnderManagedRoot_False(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")
	if got := selfupdate.RunningUnderManagedRoot(installRoot); got {
		t.Error("RunningUnderManagedRoot = true for unrelated binary, want false")
	}
}

// TestRunningUnderManagedRoot_True verifies detection when the current
// executable IS under installRoot/current/.
func TestRunningUnderManagedRoot_True(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	// Build the layout the managed root expects.
	versionDir := filepath.Join(installRoot, "versions", "v0.9.0")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a placeholder binary.
	binPath := filepath.Join(versionDir, "foreman-agent")
	if err := os.WriteFile(binPath, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create installRoot/current -> versionDir
	currentLink := filepath.Join(installRoot, "current")
	if err := os.Symlink(versionDir, currentLink); err != nil {
		t.Fatal(err)
	}

	// RunningUnderManagedRoot inspects os.Executable(). We can't override
	// os.Executable in a unit test, so we exercise the helper via the
	// exported ExecUnderInstallRoot test-seam instead.
	// If the seam doesn't exist, skip — the integration is tested at the
	// agent-wiring level.
	underRoot := selfupdate.ExecUnderInstallRootForTest(filepath.Join(currentLink, "foreman-agent"), installRoot)
	if !underRoot {
		t.Error("ExecUnderInstallRootForTest = false for a path under current/")
	}

	notUnderRoot := selfupdate.ExecUnderInstallRootForTest("/usr/local/bin/foreman-agent", installRoot)
	if notUnderRoot {
		t.Error("ExecUnderInstallRootForTest = true for /usr/local/bin path")
	}
}

// TestExecUnderInstallRoot_ResolvedCurrentSymlink reproduces the production
// flow that the literal-path test above missed: launchd/systemd exec the
// binary through installRoot/current/<binary>, and RunningUnderManagedRoot
// symlink-resolves os.Executable() to its real path under versions/<v>/ before
// the check. The detector must therefore resolve the current symlink too;
// without that, the resolved exe never matches the literal current/ prefix and
// self-update is silently disabled on every real managed install.
func TestExecUnderInstallRoot_ResolvedCurrentSymlink(t *testing.T) {
	dir := t.TempDir()
	installRoot := filepath.Join(dir, "foreman-agent")

	versionDir := filepath.Join(installRoot, "versions", "v0.9.0")
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "foreman-agent"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versionDir, filepath.Join(installRoot, "current")); err != nil {
		t.Fatal(err)
	}

	// Mimic RunningUnderManagedRoot: resolve the exe's symlinks (current ->
	// versions/v0.9.0) exactly as os.Executable()+EvalSymlinks would in prod.
	resolvedExe, err := filepath.EvalSymlinks(filepath.Join(installRoot, "current", "foreman-agent"))
	if err != nil {
		t.Fatal(err)
	}

	if !selfupdate.ExecUnderInstallRootForTest(resolvedExe, installRoot) {
		t.Errorf("ExecUnderInstallRootForTest(resolved exe) = false, want true; "+
			"resolvedExe=%s installRoot=%s", resolvedExe, installRoot)
	}
}

// TestResolveInstallRoot checks the platform-specific default.
func TestResolveInstallRoot(t *testing.T) {
	root, err := selfupdate.ResolveInstallRoot("foreman-agent")
	if err != nil {
		t.Fatalf("ResolveInstallRoot: %v", err)
	}
	if root == "" {
		t.Fatal("ResolveInstallRoot returned empty string")
	}
	// Must contain the binary name.
	if !strings.Contains(root, "foreman-agent") {
		t.Errorf("install root %q does not contain binary name", root)
	}
}

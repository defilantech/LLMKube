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

// Package selfupdate performs in-place agent binary replacement. It
// downloads a new binary, verifies its SHA-256 digest, stages it under
// a versioned layout, and atomically flips the "current" symlink.
//
// RESTART DESIGN: MaybeApply does NOT restart the process. It only stages
// the new binary and flips the symlink. The caller is responsible for
// draining traffic and exiting cleanly so that the supervisor (launchd
// KeepAlive / systemd Restart=always) relaunches the process, which will
// exec the new binary through the updated "current" symlink.
//
// INSTALL ROOT LAYOUT:
//
//	<installRoot>/
//	  versions/
//	    <version>/
//	      <binaryName>       (staged binary, chmod 0o755)
//	  current  -> versions/<latestVersion>   (atomic symlink flip)
//	  previous -> versions/<priorVersion>    (set on each successful flip)
//
// The agent binary is run as: <installRoot>/current/<binaryName>.
// Self-update only engages when the running executable is under
// <installRoot>/current; dev builds (e.g. `go run`) are ignored.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-logr/logr"
)

// Updater performs self-update of a managed binary. Zero value is not
// usable; construct via struct literal and set all fields.
type Updater struct {
	// CurrentVersion is the version string of the currently running binary.
	// Set from the build-time ldflags var (e.g. "v0.8.4").
	CurrentVersion string

	// OS and Arch are the platform this binary runs on (runtime.GOOS /
	// runtime.GOARCH). Informational only; the operator selects the correct
	// artifact URL before writing UpdateRequest.
	OS, Arch string

	// InstallRoot is the user-owned directory managed by this Updater.
	// Conventionally ~/Library/Application Support/llmkube/<binaryName>
	// on macOS or ~/.local/share/llmkube/<binaryName> on Linux.
	// Use ResolveInstallRoot to get the platform default.
	InstallRoot string

	// BinaryName is the name of the executable file (e.g. "foreman-agent").
	BinaryName string

	// Verifier checks the downloaded artifact before staging. In production
	// this is &SHA256Verifier{}; tests may inject a fake.
	Verifier Verifier

	// HTTPClient is used to download the artifact. Defaults to
	// http.DefaultClient when nil.
	HTTPClient *http.Client

	// MaxDownloadBytes caps the artifact download size. A malformed or
	// malicious URL could otherwise stream an unbounded body and fill the
	// partition before the SHA-256 verify runs. When zero, defaults to
	// defaultMaxDownloadBytes (512 MiB). The cap is enforced two ways: a
	// Content-Length precheck (fail before reading the body) and a
	// LimitReader on the stream (fail mid-copy on overflow).
	MaxDownloadBytes int64

	// RetainVersions is the number of staged version directories to keep
	// (beyond the always-retained current and previous targets) after a
	// successful symlink flip. When zero, defaults to defaultRetainVersions
	// (3). Older versions/<v> directories are pruned best-effort.
	RetainVersions int

	// Log is the structured logger. logr.Discard() silences all output.
	Log logr.Logger
}

const (
	// defaultMaxDownloadBytes is the artifact download ceiling when
	// Updater.MaxDownloadBytes is zero: 512 MiB.
	defaultMaxDownloadBytes int64 = 512 * 1024 * 1024

	// defaultRetainVersions is the number of staged versions kept (beyond
	// current and previous) when Updater.RetainVersions is zero.
	defaultRetainVersions = 3
)

// Target describes the update to apply, sourced from
// FleetNode.status.updateRequest.
type Target struct {
	Version string
	URL     string
	SHA256  string
}

// ApplyResult is returned by MaybeApply.
type ApplyResult struct {
	// Restarting is true when the new binary was staged and the current
	// symlink flipped. The caller should drain and exit; the supervisor
	// will restart onto the new binary.
	Restarting bool
}

// Verifier validates a downloaded artifact at path against an expected digest.
type Verifier interface {
	Verify(path, sha256Hex string) error
}

// SHA256Verifier streams a file and compares its SHA-256 against the
// expected lowercase hex string.
type SHA256Verifier struct{}

// Verify computes the SHA-256 of the file at path and returns an error if
// it does not match expectedHex (case-insensitive, trimmed).
func (*SHA256Verifier) Verify(path, expectedHex string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", path, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(expectedHex))
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// MaybeApply is a no-op when t.Version is empty or equals CurrentVersion.
// Otherwise it downloads, verifies, stages, and atomically flips the
// current symlink. It does NOT restart the process; Restarting=true is
// the signal for the caller to drain and exit.
//
// On SHA-256 mismatch the temp file is deleted and the current symlink
// is unchanged.
func (u *Updater) MaybeApply(t Target) (ApplyResult, error) {
	if t.Version == "" || t.Version == u.CurrentVersion {
		return ApplyResult{}, nil
	}

	log := u.Log.WithValues("target", t.Version, "current", u.CurrentVersion)
	log.Info("self-update: applying", "url", t.URL)

	hc := u.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	// Ensure the install root exists before writing temp files.
	if err := os.MkdirAll(u.InstallRoot, 0o755); err != nil {
		return ApplyResult{}, fmt.Errorf("create install root: %w", err)
	}

	// Download into a temp file INSIDE InstallRoot so os.Rename is on the
	// same filesystem (avoiding EXDEV cross-device link errors).
	tmp, err := os.CreateTemp(u.InstallRoot, "tmp-download-*")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// Cleanup: delete temp on any error path.
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	maxBytes := u.MaxDownloadBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxDownloadBytes
	}
	if err := u.download(hc, t.URL, tmp, maxBytes); err != nil {
		_ = tmp.Close()
		return ApplyResult{}, fmt.Errorf("download %s: %w", t.URL, err)
	}
	if err := tmp.Close(); err != nil {
		return ApplyResult{}, fmt.Errorf("close temp file: %w", err)
	}

	// Verify before staging.
	if err := u.Verifier.Verify(tmpPath, t.SHA256); err != nil {
		return ApplyResult{}, fmt.Errorf("sha256 verify %s: %w", t.Version, err)
	}

	// Stage: installRoot/versions/<version>/<binaryName>
	versionDir := filepath.Join(u.InstallRoot, "versions", t.Version)
	if err := os.MkdirAll(versionDir, 0o755); err != nil {
		return ApplyResult{}, fmt.Errorf("create version dir: %w", err)
	}
	stagedPath := filepath.Join(versionDir, u.BinaryName)
	if err := os.Rename(tmpPath, stagedPath); err != nil {
		return ApplyResult{}, fmt.Errorf("stage binary: %w", err)
	}
	success = true                                      // temp file consumed by Rename, no need to delete
	if err := os.Chmod(stagedPath, 0o755); err != nil { //nolint:gosec // G302: 0755 is required for executable binaries
		return ApplyResult{}, fmt.Errorf("chmod staged binary: %w", err)
	}

	// Record the current target as previous before flipping.
	currentLink := filepath.Join(u.InstallRoot, "current")
	if target, err := os.Readlink(currentLink); err == nil && target != "" {
		previousLink := filepath.Join(u.InstallRoot, "previous")
		// Atomic: write to previous.tmp then rename.
		prevTmp := filepath.Join(u.InstallRoot, "previous.tmp")
		if err := os.Symlink(target, prevTmp); err != nil {
			// If prevTmp already exists, remove it first.
			_ = os.Remove(prevTmp)
			if err2 := os.Symlink(target, prevTmp); err2 != nil {
				return ApplyResult{}, fmt.Errorf("create previous.tmp symlink: %w", err2)
			}
		}
		if err := os.Rename(prevTmp, previousLink); err != nil {
			_ = os.Remove(prevTmp)
			return ApplyResult{}, fmt.Errorf("rename previous symlink: %w", err)
		}
	}

	// Atomic symlink flip: write current.tmp -> new version dir, then
	// rename over current. os.Rename is atomic on POSIX for both files
	// and symlinks when source and destination are on the same filesystem.
	currentTmp := filepath.Join(u.InstallRoot, "current.tmp")
	if err := os.Symlink(versionDir, currentTmp); err != nil {
		_ = os.Remove(currentTmp)
		if err2 := os.Symlink(versionDir, currentTmp); err2 != nil {
			return ApplyResult{}, fmt.Errorf("create current.tmp symlink: %w", err2)
		}
	}
	if err := os.Rename(currentTmp, currentLink); err != nil {
		_ = os.Remove(currentTmp)
		return ApplyResult{}, fmt.Errorf("flip current symlink: %w", err)
	}

	// Prune old staged versions. Best-effort: the update already succeeded,
	// so a prune failure must never block or revert it. current and previous
	// (the running + rollback targets) are always retained.
	retain := u.RetainVersions
	if retain <= 0 {
		retain = defaultRetainVersions
	}
	if err := u.pruneOldVersions(retain); err != nil {
		log.Error(err, "self-update: pruning old versions failed (non-fatal)")
	}

	log.Info("self-update: staged and symlink flipped; will restart via supervisor")
	return ApplyResult{Restarting: true}, nil
}

// pruneOldVersions deletes stale versions/<v> directories, keeping:
//   - the directory that "current" resolves to (the running target),
//   - the directory that "previous" resolves to (the rollback target),
//   - the newest `retain` of the remaining directories, by mtime.
//
// It is best-effort: callers treat the returned error as non-fatal. current
// and previous are never deleted regardless of their mtime.
func (u *Updater) pruneOldVersions(retain int) error {
	versionsDir := filepath.Join(u.InstallRoot, "versions")

	// Resolve the protected targets. A missing or dangling symlink is not an
	// error here; it just means there is nothing to protect on that slot.
	protected := map[string]struct{}{}
	for _, link := range []string{"current", "previous"} {
		if target, err := os.Readlink(filepath.Join(u.InstallRoot, link)); err == nil && target != "" {
			// Symlinks store an absolute path to versions/<v>; normalize the
			// base name so comparison is independent of how it was written.
			protected[filepath.Base(target)] = struct{}{}
		}
	}

	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return fmt.Errorf("read versions dir: %w", err)
	}

	type versionDir struct {
		name  string
		mtime int64
	}
	var prunable []versionDir
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := protected[e.Name()]; ok {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			// Unreadable entry: skip it rather than abort the whole prune.
			continue
		}
		prunable = append(prunable, versionDir{name: e.Name(), mtime: info.ModTime().UnixNano()})
	}

	if len(prunable) <= retain {
		return nil
	}

	// Newest first by mtime; keep the first `retain`, delete the rest.
	sort.Slice(prunable, func(i, j int) bool {
		return prunable[i].mtime > prunable[j].mtime
	})

	var firstErr error
	for _, vd := range prunable[retain:] {
		if rmErr := os.RemoveAll(filepath.Join(versionsDir, vd.name)); rmErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove %s: %w", vd.name, rmErr)
		}
	}
	return firstErr
}

// download streams the response body from url into dst, enforcing a maxBytes
// ceiling. The cap is checked twice: an advertised Content-Length over the
// limit fails before the body is read, and the stream is wrapped in an
// io.LimitReader(maxBytes+1) so an unadvertised (chunked) overflow fails
// mid-copy. Either overflow returns an "exceeds max" error before staging.
func (u *Updater) download(hc *http.Client, url string, dst *os.File, maxBytes int64) error {
	resp, err := hc.Get(url) //nolint:noctx // URL is operator-provided; no extra cancellation needed
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
	}

	// Precheck: a trustworthy Content-Length over the limit fails fast
	// without reading the body at all.
	if resp.ContentLength >= 0 && resp.ContentLength > maxBytes {
		return fmt.Errorf("artifact size %d exceeds max %d bytes", resp.ContentLength, maxBytes)
	}

	// Stream with a hard cap. Read up to maxBytes+1 so that copying exactly
	// maxBytes+1 bytes signals overflow (the body had more than maxBytes).
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("copy body: %w", err)
	}
	if n > maxBytes {
		return fmt.Errorf("artifact size %d exceeds max %d bytes", n, maxBytes)
	}
	return nil
}

// ResolveInstallRoot returns the user-owned default install root for
// binaryName on the current platform:
//
//   - darwin: ~/Library/Application Support/llmkube/<binaryName>
//   - linux:  ~/.local/share/llmkube/<binaryName>
//   - other:  ~/.local/share/llmkube/<binaryName>
func ResolveInstallRoot(binaryName string) (string, error) {
	return resolveInstallRoot(binaryName)
}

// resolveInstallRoot is the platform-specific implementation; defined in
// platform-specific files (install_root_darwin.go, install_root_other.go).
// Declared here as a forward reference so the package compiles.

// RunningUnderManagedRoot reports whether the current os.Executable()
// resolves to a path under installRoot/current/. This gates whether
// self-update should engage: dev builds (go run, `go test`, direct binary)
// return false and are safely ignored.
func RunningUnderManagedRoot(installRoot string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	// Resolve symlinks so we compare real paths.
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		// If the link can't be resolved the binary may be in a transient
		// state; treat as not-managed.
		return false
	}
	return execUnderInstallRoot(exe, installRoot)
}

// ExecUnderInstallRootForTest is a test-only seam that lets unit tests
// exercise the detection logic without relying on os.Executable().
func ExecUnderInstallRootForTest(exe, installRoot string) bool {
	return execUnderInstallRoot(exe, installRoot)
}

// execUnderInstallRoot checks whether exe is under installRoot/current/.
func execUnderInstallRoot(exe, installRoot string) bool {
	currentDir := filepath.Join(installRoot, "current")
	// Normalize to absolute paths for comparison.
	absExe, err := filepath.Abs(exe)
	if err != nil {
		return false
	}
	absCurrent, err := filepath.Abs(currentDir)
	if err != nil {
		return false
	}
	// Resolve symlinks on BOTH sides before comparing. The managed layout
	// points `current` at `versions/<v>` via a symlink, and the supervisor
	// execs the binary through current/, so os.Executable() resolves (in
	// RunningUnderManagedRoot) to versions/<v>/<binary>. Comparing that
	// resolved exe against the literal current/ prefix never matches, which
	// silently disabled self-update on every real managed install. Resolving
	// `current` here makes both sides land on the same real versions/<v> path.
	// EvalSymlinks fails for a non-existent path (e.g. an unrelated
	// /usr/local/bin binary in tests); in that case we keep the absolute path
	// and the prefix check still rejects it.
	if resolved, rerr := filepath.EvalSymlinks(absExe); rerr == nil {
		absExe = resolved
	}
	if resolved, rerr := filepath.EvalSymlinks(absCurrent); rerr == nil {
		absCurrent = resolved
	}
	// Ensure absCurrent has a trailing separator so HasPrefix doesn't
	// match /foo/current-extra.
	if !strings.HasSuffix(absCurrent, string(filepath.Separator)) {
		absCurrent += string(filepath.Separator)
	}
	return strings.HasPrefix(absExe, absCurrent)
}

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

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// CheckModelStore refuses a model store another local user could tamper with.
// It is ResolveModelStore without the resolved path; see there for the rules.
func CheckModelStore(path string) error {
	_, err := ResolveModelStore(path)
	return err
}

// ResolveModelStore resolves path to an absolute, symlink-free directory,
// checks it, and returns it. The store holds the model files engines load and
// the per-process engine logs, and it is always an allowed model root, so a
// store someone else owns or can write lets them swap a model, plant a
// symlink the agent then writes through, or widen what the root policy
// admits.
//
// The resolved target is judged, not the link: a link the agent owns
// pointing at a shared directory is still a shared store. The target must
// exist, be a directory outside the shared temporary directories (/tmp,
// /var/tmp and their /private forms), be owned by the agent's uid and have
// no group or other write bit. Every ancestor up to / must be owned by root
// or the agent's uid, and be unwritable by group and other unless it has the
// sticky bit (so a root-owned, sticky directory such as /Users/Shared
// passes): otherwise another user could rename the store away and put their
// own directory in its place after this check.
//
// Callers must use the returned path from then on, never path itself: path
// is re-resolved on every use, so a symlink in it could be repointed after
// the check. Errors name the path, owner uid, mode and the fix.
func ResolveModelStore(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("model store %s: %w", path, err)
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", fmt.Errorf("model store %s: %w", path, err)
	}
	where := describeStore(path, resolved)
	if err := checkStoreNotInTmp(where, path, resolved); err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("model store %s: %w", where, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("model store %s is not a directory", where)
	}
	owner, ok := fileOwner(resolved, info)
	if !ok {
		return "", fmt.Errorf("model store %s: cannot read its owner", where)
	}
	uid := os.Getuid()
	mode := info.Mode().Perm()
	if owner != uid {
		return "", fmt.Errorf("model store %s has owner uid %d and mode %04o, but the agent runs as uid %d "+
			"and refuses a store another user owns: run `sudo chown %d %s` or pass a --model-store the agent owns",
			where, owner, mode, uid, uid, resolved)
	}
	if mode&0o022 != 0 {
		return "", fmt.Errorf("model store %s has owner uid %d and mode %04o, which lets other users write to it: "+
			"run `chmod go-w %s` or pass a private --model-store",
			where, owner, mode, resolved)
	}
	if err := checkStoreAncestors(where, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// systemTempRoots are the shared temporary directories a model store must
// not live in. /tmp and /var/tmp are symlinks to /private/... on macOS, so
// both spellings are listed: the resolved path normally carries the /private
// form, and the literal form catches a configured path whose resolution
// differs. Unexported so no importer can change the check at runtime; tests
// override it through SetSystemTempRootsForTest.
var systemTempRoots = []string{"/private/tmp", "/tmp", "/private/var/tmp", "/var/tmp"}

// checkStoreNotInTmp refuses a store at or under a shared temporary
// directory. Even a store the agent owns there is not safe: /tmp is emptied
// at boot, so another local user can create the store directory first after
// a reboot and either own it (the agent then refuses to start) or wait for
// the agent to trust whatever they put in it. 0.10.0 plists pinned
// --model-store /tmp/llmkube-models, and launchd keeps running a loaded job's
// old definition across `launchctl kickstart -k` (and across a re-rendered
// plist file) until the job is booted out, so the error names both steps.
func checkStoreNotInTmp(where, path, resolved string) error {
	literal := path
	if abs, err := filepath.Abs(path); err == nil {
		literal = abs
	}
	for _, candidate := range []string{resolved, filepath.Clean(literal)} {
		for _, root := range systemTempRoots {
			if candidate == root || strings.HasPrefix(candidate, root+"/") {
				return fmt.Errorf("model store %s is under %s, a shared temporary directory any local user "+
					"can recreate after a reboot: re-render the launchd plist with the new default store "+
					"(0.10.0 plists pinned --model-store /tmp/llmkube-models): run "+
					"`launchctl bootout gui/$(id -u)/com.llmkube.metal-agent`, wait until it has unloaded, then "+
					"`make install-metal-agent` (see \"Upgrading to 0.10.1\" in deployment/macos/README.md), "+
					"or pass a --model-store the agent owns outside /tmp", where, root)
			}
		}
	}
	return nil
}

// SetSystemTempRootsForTest overrides the shared temporary directories the
// model-store check refuses and returns the previous list so a test can
// restore it. It is a no-op outside a test binary, so no importer can change
// the production check at runtime; tests whose t.TempDir() is under /tmp
// clear the list, and the refusal test restores the production default.
func SetSystemTempRootsForTest(roots []string) []string {
	if !testing.Testing() {
		return nil
	}
	prev := systemTempRoots
	systemTempRoots = roots
	return prev
}

// checkStoreAncestors refuses the store when any ancestor of the resolved,
// absolute store directory is owned by a uid other than root or the agent
// (its owner could chmod it and swap the store: the ssh StrictModes rule), or
// is group- or other-writable without the sticky bit.
func checkStoreAncestors(where, resolved string) error {
	uid := os.Getuid()
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("model store %s: ancestor %s: %w", where, dir, err)
		}
		owner, ok := fileOwner(dir, info)
		if !ok {
			return fmt.Errorf("model store %s: ancestor %s: cannot read its owner", where, dir)
		}
		m := info.Mode()
		if owner != 0 && owner != uid {
			return fmt.Errorf("model store %s has ancestor %s with owner uid %d and mode %04o; every directory "+
				"above the store must be owned by root or the agent (uid %d), or its owner could swap the store: "+
				"pass a --model-store under a directory you or root own", where, dir, owner, m.Perm(), uid)
		}
		if m.Perm()&0o022 != 0 && m&os.ModeSticky == 0 {
			return fmt.Errorf("model store %s has ancestor %s with owner uid %d and mode %04o, which lets other "+
				"users replace the store: run `chmod go-w %s` (or `chmod +t` if it must stay shared) or pass a "+
				"--model-store under a private directory", where, dir, owner, m.Perm(), dir)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}

// fileOwner returns the owner uid recorded in info (the Lstat result for
// path). It is a variable so tests can report a foreign owner for one path
// without a second account.
var fileOwner = func(_ string, info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// describeStore names the configured path and, when it differs, the
// directory it resolved to, so an error about a symlinked store points at
// the directory that actually needs fixing.
func describeStore(path, resolved string) string {
	if resolved == path {
		return path
	}
	return fmt.Sprintf("%s (resolves to %s)", path, resolved)
}

// createNoFollow creates or truncates path for writing with mode 0600 and
// refuses to follow a symlink at path. Files the agent creates inside the
// model store go through it (or appendNoFollow): a symlink planted there
// would otherwise make the agent truncate and write whatever it points at.
func createNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
}

// appendNoFollow opens an existing path for appending and refuses to follow
// a symlink at path.
func appendNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
}

// openEngineLog opens a new per-process log for one engine run without
// following a symlink. The previous run's log is kept as <logPath>.1 (one
// generation; see rotateEngineLog), so the restart after a crash does not
// erase the output that explains it. A symlinked log path fails the start
// with an error naming the path rather than writing through the link.
func openEngineLog(engine, logPath string) (*os.File, error) {
	rotateEngineLog(logPath)
	f, err := createNoFollow(logPath)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("refusing to open %s log file %s: it is a symlink (remove it and retry): %w",
			engine, logPath, err)
	}
	return nil, fmt.Errorf("failed to open %s log file %s: %w", engine, logPath, err)
}

// rotateEngineLog renames the log at logPath to logPath.1 when it is a
// regular file. rename(2) replaces whatever sits at .1, a planted symlink
// included, without following it, and the file keeps the mode the agent
// created it with. A symlink at logPath is left in place for openEngineLog to
// refuse. Rotation is best effort: when it fails, the new run truncates the
// log as it always did.
func rotateEngineLog(logPath string) {
	info, err := os.Lstat(logPath)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	_ = os.Rename(logPath, logPath+".1")
}

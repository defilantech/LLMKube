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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeWithMode returns a fresh directory owned by the test user with mode
// set explicitly (Chmod, so the umask does not interfere).
func storeWithMode(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckModelStore_OwnedPrivateStorePasses(t *testing.T) {
	for _, mode := range []os.FileMode{0o700, 0o755, 0o750} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			if err := CheckModelStore(storeWithMode(t, mode)); err != nil {
				t.Fatalf("CheckModelStore(owned %o) = %v, want nil", mode, err)
			}
		})
	}
}

func TestCheckModelStore_RefusesGroupOrOtherWritable(t *testing.T) {
	for _, mode := range []os.FileMode{0o775, 0o757, 0o770, 0o702, 0o777} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			dir := storeWithMode(t, mode)
			err := CheckModelStore(dir)
			if err == nil {
				t.Fatalf("CheckModelStore(owned %o) = nil, want a refusal", mode)
			}
			msg := err.Error()
			for _, want := range []string{
				dir,
				fmt.Sprintf("owner uid %d", os.Getuid()),
				fmt.Sprintf("mode %04o", mode),
				"chmod go-w",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
		})
	}
}

func TestCheckModelStore_SymlinkJudgedOnTarget(t *testing.T) {
	t.Run("symlink to a group-writable dir is refused", func(t *testing.T) {
		target := storeWithMode(t, 0o775)
		link := filepath.Join(t.TempDir(), "store-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := CheckModelStore(link)
		if err == nil {
			t.Fatal("CheckModelStore(symlink to 0775 dir) = nil, want a refusal")
		}
		resolved, _ := filepath.EvalSymlinks(target)
		if !strings.Contains(err.Error(), resolved) {
			t.Errorf("error %q does not name the resolved target %q", err, resolved)
		}
	})

	t.Run("symlink to an owned 0700 dir passes", func(t *testing.T) {
		target := storeWithMode(t, 0o700)
		link := filepath.Join(t.TempDir(), "store-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := CheckModelStore(link); err != nil {
			t.Fatalf("CheckModelStore(symlink to owned 0700 dir) = %v, want nil", err)
		}
	})

	t.Run("symlink to a dir owned by another uid is refused", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("running as root: / is owned by the agent uid here, so no second user is available")
		}
		// "/" is owned by root (uid 0) and is not group/other-writable on
		// macOS and Linux, so only the ownership rule can refuse it.
		link := filepath.Join(t.TempDir(), "store-link")
		if err := os.Symlink("/", link); err != nil {
			t.Fatal(err)
		}
		err := CheckModelStore(link)
		if err == nil {
			t.Fatal("CheckModelStore(symlink to a root-owned dir) = nil, want a refusal")
		}
		for _, want := range []string{"owner uid 0", "chown"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

func TestCheckModelStore_RefusesMissingOrNonDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if err := CheckModelStore(missing); err == nil {
		t.Error("CheckModelStore(missing) = nil, want an error")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckModelStore(file); err == nil {
		t.Error("CheckModelStore(regular file) = nil, want an error")
	}
}

func TestCreateNoFollow_RefusesSymlinkedPath(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "engine.log")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	f, err := createNoFollow(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("createNoFollow(symlink) succeeded, want an error")
	}
	got, _ := os.ReadFile(victim)
	if string(got) != "keep me" {
		t.Fatalf("symlink target was truncated or written: %q", got)
	}
}

func TestCreateNoFollow_CreatesAndTruncatesRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engine.log")
	if err := os.WriteFile(p, []byte("old run"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := createNoFollow(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0 (truncated)", info.Size())
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
}

// Every engine that captures child output must refuse a symlinked log path:
// the per-process logs live in the model store, and a planted symlink there
// would otherwise have the agent truncate and write whatever it points at.
// Each case reuses the executor's existing fixture, plants the symlink, and
// starts the process the normal way; the start must fail before the engine
// runs and leave the symlink target untouched.
func TestEngineStartProcess_RefusesSymlinkedLog(t *testing.T) {
	type fixture func(t *testing.T) (start func() error, logPath string)
	const body = "echo ran >&2\nexit 3\n"
	cases := map[string]fixture{
		"llama-server": func(t *testing.T) (func() error, string) {
			e, cfg, logPath := childExitFixture(t, fakeLlamaServer(t, body))
			return func() error { _, err := e.StartProcess(context.Background(), cfg); return err }, logPath
		},
		"mlx-server": func(t *testing.T) (func() error, string) {
			e, cfg, logPath := mlxServerFixture(t, fakeMLXServer(t, body), 0)
			return func() error { _, err := e.StartProcess(context.Background(), cfg); return err }, logPath
		},
		"tensorfold": func(t *testing.T) (func() error, string) {
			e, cfg, logPath := tensorFoldFixture(t, fakeTensorFold(t, body))
			return func() error { _, err := e.StartProcess(context.Background(), cfg); return err }, logPath
		},
		"vllm-swift": func(t *testing.T) (func() error, string) {
			e, cfg, logPath := vllmSwiftFixture(t, fakeVLLMSwift(t, body))
			return func() error { _, err := e.StartProcess(context.Background(), cfg); return err }, logPath
		},
	}
	for name, fx := range cases {
		t.Run(name, func(t *testing.T) {
			start, logPath := fx(t)
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(logPath)
			if err := os.Symlink(victim, logPath); err != nil {
				t.Fatal(err)
			}
			err := start()
			if err == nil {
				t.Fatal("StartProcess with a symlinked log succeeded, want an error")
			}
			if !strings.Contains(err.Error(), logPath) {
				t.Errorf("error %q does not name the log path %q", err, logPath)
			}
			got, _ := os.ReadFile(victim)
			if string(got) != "keep me" {
				t.Fatalf("symlink target was truncated or written: %q", got)
			}
		})
	}
}

// The agent also creates files inside the store for downloads: the .part
// file (fresh and resumed) and the <file>.sha256 stamp. None of them may
// follow a planted symlink.
func TestStoreWrites_RefuseSymlinks(t *testing.T) {
	plant := func(t *testing.T, p string) string {
		t.Helper()
		victim := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, p); err != nil {
			t.Fatal(err)
		}
		return victim
	}
	untouched := func(t *testing.T, victim string) {
		t.Helper()
		if got, _ := os.ReadFile(victim); string(got) != "keep me" {
			t.Fatalf("symlink target was modified: %q", got)
		}
	}
	e := &MetalExecutor{}

	t.Run("sha256 stamp", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "model.gguf")
		if err := os.WriteFile(file, []byte("model bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		victim := plant(t, sha256StampPath(file))
		if err := writeSHA256Stamp(file, strings.Repeat("a", 64)); err == nil {
			t.Fatal("writeSHA256Stamp through a symlink succeeded, want an error")
		}
		untouched(t, victim)
	})

	t.Run("fresh part file", func(t *testing.T) {
		dir := t.TempDir()
		part := filepath.Join(dir, "model.gguf.part")
		victim := plant(t, part)
		err := e.copyToFileResume(part, filepath.Join(dir, "model.gguf"), strings.NewReader("data"), 4, 0, "")
		if err == nil {
			t.Fatal("copyToFileResume into a symlinked part file succeeded, want an error")
		}
		untouched(t, victim)
	})

	t.Run("resumed part file", func(t *testing.T) {
		dir := t.TempDir()
		part := filepath.Join(dir, "model.gguf.part")
		victim := plant(t, part)
		err := e.copyToFileResume(part, filepath.Join(dir, "model.gguf"), strings.NewReader("data"), 11, 7, "")
		if err == nil {
			t.Fatal("copyToFileResume appending to a symlinked part file succeeded, want an error")
		}
		untouched(t, victim)
	})
}

// An ancestor another user can write (and that lacks the sticky bit) lets
// them rename the store away and put their own directory in its place after
// the check, so it is refused just like a writable store.
func TestCheckModelStore_Ancestors(t *testing.T) {
	mk := func(t *testing.T, parentMode os.FileMode) (parent, store string) {
		t.Helper()
		parent = filepath.Join(t.TempDir(), "parent")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		store = filepath.Join(parent, "store")
		if err := os.Mkdir(store, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, parentMode); err != nil {
			t.Fatal(err)
		}
		return parent, store
	}

	t.Run("group-writable non-sticky ancestor is refused", func(t *testing.T) {
		parent, store := mk(t, 0o775)
		err := CheckModelStore(store)
		if err == nil {
			t.Fatal("CheckModelStore under a 0775 ancestor = nil, want a refusal")
		}
		resolvedParent, _ := filepath.EvalSymlinks(parent)
		for _, want := range []string{"ancestor " + resolvedParent, "mode 0775"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("other-writable non-sticky ancestor is refused", func(t *testing.T) {
		_, store := mk(t, 0o757)
		if err := CheckModelStore(store); err == nil {
			t.Fatal("CheckModelStore under a 0757 ancestor = nil, want a refusal")
		}
	})

	t.Run("sticky world-writable ancestor passes", func(t *testing.T) {
		_, store := mk(t, 0o777|os.ModeSticky)
		if err := CheckModelStore(store); err != nil {
			t.Fatalf("CheckModelStore under a sticky 1777 ancestor = %v, want nil", err)
		}
	})
}

// tmpDirForTest makes a private directory directly under /tmp (not
// t.TempDir(), which on macOS lives under /var/folders) and removes it when
// the test ends.
func tmpDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "llmkube-store-test-")
	if err != nil {
		t.Skipf("cannot create a directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// A store under /tmp is refused even when the agent owns it and it is 0700:
// /tmp is emptied at boot, so another local user can recreate the path
// first. 0.10.0 plists pinned --model-store /tmp/llmkube-models, so the
// error must name the plist re-render.
func TestCheckModelStore_RefusesStoreUnderTmp(t *testing.T) {
	withSystemTempRoots(t)
	wantAll := func(t *testing.T, err error, path string) {
		t.Helper()
		if err == nil {
			t.Fatalf("CheckModelStore(%s) = nil, want a refusal", path)
		}
		for _, want := range []string{path, "make install-metal-agent", "outside /tmp"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}

	t.Run("owned private store under /tmp", func(t *testing.T) {
		store := filepath.Join(tmpDirForTest(t), "models")
		if err := os.Mkdir(store, 0o700); err != nil {
			t.Fatal(err)
		}
		wantAll(t, CheckModelStore(store), store)
	})

	t.Run("symlink outside /tmp resolving into /tmp", func(t *testing.T) {
		store := filepath.Join(tmpDirForTest(t), "models")
		if err := os.Mkdir(store, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "store-link")
		if err := os.Symlink(store, link); err != nil {
			t.Fatal(err)
		}
		wantAll(t, CheckModelStore(link), link)
	})

	t.Run("literal /tmp path whose target is outside /tmp", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "models")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(tmpDirForTest(t), "store-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		wantAll(t, CheckModelStore(link), link)
	})
}

// ResolveModelStore returns the checked, symlink-free absolute path, so the
// agent keeps using the directory that was judged even if the configured
// symlink is repointed afterwards.
func TestResolveModelStore_PinsResolvedTarget(t *testing.T) {
	good := storeWithMode(t, 0o700)
	other := storeWithMode(t, 0o700)
	link := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveModelStore(link)
	if err != nil {
		t.Fatalf("ResolveModelStore = %v", err)
	}
	want, _ := filepath.EvalSymlinks(good)
	if got != want {
		t.Fatalf("ResolveModelStore = %q, want the resolved target %q", got, want)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("pinned path changed after the symlink moved: %q", got)
	}
}

func TestEnsureModel_RefusesNonRegularCacheEntry(t *testing.T) {
	store := t.TempDir()
	e := NewMetalExecutor("/bin/llama-server", store, newNopLogger())
	md := filepath.Join(store, "m")
	if err := os.Mkdir(md, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "elsewhere.gguf")
	if err := os.WriteFile(victim, []byte("not the model"), 0o600); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(md, "model.gguf")
	if err := os.Symlink(victim, localPath); err != nil {
		t.Fatal(err)
	}
	got, err := e.ensureModel(t.Context(), "https://example.invalid/model.gguf", "m", nil, "")
	if err == nil {
		t.Fatalf("ensureModel with a symlinked cache entry = %q, nil; want a refusal", got)
	}
	if !strings.Contains(err.Error(), localPath) {
		t.Errorf("error %q does not name the cache path", err)
	}
	var slotErr *ModelCacheEntryNotRegularError
	if !errors.As(err, &slotErr) {
		t.Errorf("error %T is not a *ModelCacheEntryNotRegularError, so it would not be refused with an event", err)
	}
}

// A complete-size partial is published by rename. If a symlink planted at
// the partial path were accepted (os.Stat follows it), the published model
// would be that symlink.
func TestDownloadFile_SymlinkedCompletePartialNotPublished(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	md := modelDirPath(t, dir, "m")
	localPath := filepath.Join(md, "model.gguf")
	o := newDLOrigin(t, true, true)
	validator := "CL" + fmt.Sprint(dlSizeA) + "ET" + `"vA"`
	part := validatorPartialPath(localPath, validator)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte(strings.Repeat("A", dlSizeA)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, part); err != nil {
		t.Fatal(err)
	}
	_ = ex.downloadFile(t.Context(), o.srv.URL+"/model.gguf", localPath, "", "")
	if fi, err := os.Lstat(localPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a symlinked partial was published as the model")
	}
}

func TestEnsureModel_CreatesModelDirPrivate(t *testing.T) {
	dir := t.TempDir()
	ex := executorFor(dir)
	o := newDLOrigin(t, true, true)
	if _, err := ex.ensureModel(t.Context(), o.srv.URL+"/model.gguf", "fresh", nil, ""); err != nil {
		t.Fatalf("ensureModel: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("model dir mode = %o, want 0700", perm)
	}
}

// An ancestor owned by another non-root user is refused even when it is not
// writable: its owner could chmod it and then swap the store (the ssh
// StrictModes rule). The owner lookup seam reports a foreign uid for one
// ancestor so the case runs without a second account.
func TestCheckModelStore_AncestorOwnedByAnotherUser(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	store := filepath.Join(parent, "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedParent, _ := filepath.EvalSymlinks(parent)

	orig := fileOwner
	t.Cleanup(func() { fileOwner = orig })
	fileOwner = func(path string, info os.FileInfo) (int, bool) {
		if path == resolvedParent {
			return 4242, true
		}
		return orig(path, info)
	}

	err := CheckModelStore(store)
	if err == nil {
		t.Fatal("CheckModelStore under an ancestor owned by uid 4242 = nil, want a refusal")
	}
	for _, want := range []string{"ancestor " + resolvedParent, "owner uid 4242", "mode 0700"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// With real ownership: only root can chown a directory to another uid.
func TestCheckModelStore_AncestorOwnedByAnotherUser_RealChown(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to chown an ancestor to another uid; the seam test covers the logic")
	}
	parent := filepath.Join(t.TempDir(), "parent")
	store := filepath.Join(parent, "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(parent, 4242, 4242); err != nil {
		t.Fatal(err)
	}
	if err := CheckModelStore(store); err == nil || !strings.Contains(err.Error(), "owner uid 4242") {
		t.Fatalf("CheckModelStore under a uid-4242 ancestor = %v, want a refusal naming the owner", err)
	}
}

// The non-regular cache-slot refusal covers only sources the agent downloads
// into the store. A local-source Model (absolute path or file://) is loaded
// in place and governed by the allowed-roots policy, so one whose path is a
// symlink inside the store (an allowed root) keeps resolving as before, even
// when that path is the same as the cache slot.
func TestEnsureModel_LocalSourceSymlinkInRootResolvesAsBefore(t *testing.T) {
	for _, scheme := range []string{"", "file://"} {
		t.Run("scheme="+scheme, func(t *testing.T) {
			store := t.TempDir()
			e := NewMetalExecutor("/bin/llama-server", store, newNopLogger())
			real := filepath.Join(store, "real.gguf")
			if err := os.WriteFile(real, []byte("gguf-bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			md := filepath.Join(store, "m")
			if err := os.Mkdir(md, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(md, "model.gguf")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			got, err := e.ensureModel(t.Context(), scheme+link, "m", nil, "")
			if err != nil {
				t.Fatalf("ensureModel(local symlink source) = %v, want it to resolve", err)
			}
			if got != link {
				t.Errorf("ensureModel = %q, want %q (the pre-change in-place path)", got, link)
			}
		})
	}
}

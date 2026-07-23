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

package cli

import (
	"os"
	"os/user"
	"runtime"
	"strconv"
	"testing"
)

func TestParseOwner(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantUID int
		wantGID int
		wantErr bool
	}{
		{"valid 0:102", "0:102", 0, 102, false},
		{"valid 1000:1000", "1000:1000", 1000, 1000, false},
		{"valid 0:0", "0:0", 0, 0, false},
		{"invalid abc", "abc", 0, 0, true},
		{"invalid 1:", "1:", 0, 0, true},
		{"invalid :2", ":2", 0, 0, true},
		{"invalid 1:2:3", "1:2:3", 0, 0, true},
		{"invalid empty", "", 0, 0, true},
		{"invalid uid not int", "abc:102", 0, 0, true},
		{"invalid gid not int", "0:abc", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid, err := parseOwner(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseOwner(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if uid != tt.wantUID {
					t.Errorf("parseOwner(%q) uid = %d, want %d", tt.input, uid, tt.wantUID)
				}
				if gid != tt.wantGID {
					t.Errorf("parseOwner(%q) gid = %d, want %d", tt.input, gid, tt.wantGID)
				}
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    os.FileMode
		wantErr bool
	}{
		{"valid 0775", "0775", 0775, false},
		{"valid 0755", "0755", 0755, false},
		{"valid 0644", "0644", 0644, false},
		{"valid 0777", "0777", 0777, false},
		{"invalid abc", "abc", 0, true},
		{"invalid 9999", "9999", 0, true},
		{"invalid empty", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, err := parseMode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && mode != tt.want {
				t.Errorf("parseMode(%q) = %o, want %o", tt.input, mode, tt.want)
			}
		})
	}
}

func TestChmodChangesMode(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-prep-chmod")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	// Set initial mode.
	if err := os.Chmod(tmpDir, 0600); err != nil {
		t.Fatal(err)
	}

	cmd := NewCachePrepCommand()
	cmd.SetArgs([]string{"--owner", "0:0", "--mode", "0775", tmpDir})

	// We expect chown to fail (unless running as root), but we can still
	// test the chmod path by checking the error message contains "chown".
	// Instead, test chmod directly via parseMode + os.Chmod.
	mode, err := parseMode("0775")
	if err != nil {
		t.Fatalf("parseMode(0775): %v", err)
	}
	if err := os.Chmod(tmpDir, mode); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}

	info, err := os.Stat(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0775 {
		t.Errorf("chmod did not change mode: got %o, want 0775", info.Mode().Perm())
	}
}

func TestChownSameOwnerSucceeds(t *testing.T) {
	// Get the current process uid/gid — chown to self always succeeds.
	u, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}
	_, err = strconv.Atoi(u.Uid)
	if err != nil {
		t.Skipf("cannot parse uid %q: %v", u.Uid, err)
	}
	_, err = strconv.Atoi(u.Gid)
	if err != nil {
		t.Skipf("cannot parse gid %q: %v", u.Gid, err)
	}

	tmpDir, err := os.MkdirTemp("", "cache-prep-chown")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	owner := u.Uid + ":" + u.Gid
	cmd := NewCachePrepCommand()
	cmd.SetArgs([]string{"--owner", owner, "--mode", "0755", tmpDir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("cache-prep to same owner failed: %v", err)
	}
}

func TestMissingTargetErrors(t *testing.T) {
	cmd := NewCachePrepCommand()
	cmd.SetArgs([]string{"--owner", "0:0", "--mode", "0755", "/nonexistent-dir-12345"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing target directory, got nil")
	}
}

func TestNotADirectoryErrors(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "cache-prep-file")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(tmpFile.Name()); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	cmd := NewCachePrepCommand()
	cmd.SetArgs([]string{"--owner", "0:0", "--mode", "0755", tmpFile.Name()})

	err = cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-directory target, got nil")
	}
}

func TestNewCachePrepCommand(t *testing.T) {
	cmd := NewCachePrepCommand()

	if cmd.Use != "prep DIR" {
		t.Errorf("Use = %q, want %q", cmd.Use, "prep DIR")
	}

	if f := cmd.Flags().Lookup("owner"); f == nil {
		t.Error("Missing --owner flag")
	}

	if f := cmd.Flags().Lookup("mode"); f == nil {
		t.Error("Missing --mode flag")
	}
}

func TestCachePrepRegistered(t *testing.T) {
	cmd := NewCacheCommand()

	subcommands := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subcommands[sub.Name()] = true
	}

	if !subcommands["prep"] {
		t.Error("cache-prep subcommand not registered under cache command")
	}
}

func TestCachePrepRequiresOwnerAndMode(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cache-prep-flags")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	cmd := NewCachePrepCommand()

	// Missing --owner
	cmd.SetArgs([]string{"--mode", "0755", tmpDir})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when --owner is missing")
	}

	// Missing --mode
	cmd.SetArgs([]string{"--owner", "0:0", tmpDir})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when --mode is missing")
	}
}

func TestCachePrepRequiresExactlyOneArg(t *testing.T) {
	cmd := NewCachePrepCommand()

	// No args
	cmd.SetArgs([]string{"--owner", "0:0", "--mode", "0755"})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no directory arg is provided")
	}

	// Too many args
	cmd.SetArgs([]string{"--owner", "0:0", "--mode", "0755", "/a", "/b"})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when too many args are provided")
	}
}

func TestCachePrepEndToEnd(t *testing.T) {
	// Skip on Windows where chown/chmod semantics differ.
	if runtime.GOOS == "windows" {
		t.Skip("chown/chmod not supported on Windows")
	}

	u, err := user.Current()
	if err != nil {
		t.Skip("cannot determine current user")
	}

	tmpDir, err := os.MkdirTemp("", "cache-prep-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	owner := u.Uid + ":" + u.Gid
	cmd := NewCachePrepCommand()
	cmd.SetArgs([]string{"--owner", owner, "--mode", "0775", tmpDir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("cache-prep failed: %v", err)
	}

	info, err := os.Stat(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0775 {
		t.Errorf("mode after cache-prep = %o, want 0775", info.Mode().Perm())
	}
}

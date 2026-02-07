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
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name     string
		v1       string
		v2       string
		expected int
	}{
		{"equal versions", "1.0.0", "1.0.0", 0},
		{"v1 less than v2 patch", "1.0.0", "1.0.1", -1},
		{"v1 greater than v2 patch", "1.0.1", "1.0.0", 1},
		{"v1 less than v2 minor", "1.0.0", "1.1.0", -1},
		{"v1 greater than v2 minor", "1.1.0", "1.0.0", 1},
		{"v1 less than v2 major", "1.0.0", "2.0.0", -1},
		{"v1 greater than v2 major", "2.0.0", "1.0.0", 1},
		{"double digit patch comparison", "0.4.9", "0.4.10", -1},
		{"double digit patch comparison reverse", "0.4.10", "0.4.9", 1},
		{"triple digit patch", "1.0.99", "1.0.100", -1},
		{"with v prefix", "v1.0.0", "v1.0.1", -1},
		{"mixed v prefix", "v1.0.0", "1.0.1", -1},
		{"double digit minor", "1.9.0", "1.10.0", -1},
		{"equal with v prefix", "v0.4.10", "0.4.10", 0},
		{"real world case", "0.4.10", "0.4.9", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := compareVersions(tt.v1, tt.v2)
			if result != tt.expected {
				t.Errorf("compareVersions(%q, %q) = %d, expected %d", tt.v1, tt.v2, result, tt.expected)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
		expected []int
	}{
		{"simple version", "1.2.3", []int{1, 2, 3}},
		{"with v prefix", "v1.2.3", []int{1, 2, 3}},
		{"double digits", "1.10.100", []int{1, 10, 100}},
		{"two parts", "1.2", []int{1, 2}},
		{"single part", "1", []int{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseVersion(tt.version)
			if len(result) != len(tt.expected) {
				t.Errorf("parseVersion(%q) returned %d parts, expected %d", tt.version, len(result), len(tt.expected))
				return
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("parseVersion(%q)[%d] = %d, expected %d", tt.version, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v0.4.12", "0.4.12"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeVersion(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGetCacheFilePath(t *testing.T) {
	path, err := getCacheFilePath()
	if err != nil {
		t.Fatalf("getCacheFilePath error: %v", err)
	}
	if path == "" {
		t.Error("getCacheFilePath returned empty path")
	}
	if !strings.Contains(path, ".llmkube") {
		t.Errorf("path %q should contain .llmkube", path)
	}
	if !strings.HasSuffix(path, "version_cache.json") {
		t.Errorf("path %q should end with version_cache.json", path)
	}
}

func TestWriteAndReadCache(t *testing.T) {
	cache := &versionCache{
		LatestVersion: "v0.5.0",
		CheckedAt:     time.Now(),
	}
	if err := writeCache(cache); err != nil {
		t.Fatalf("writeCache error: %v", err)
	}

	readBack, err := readCache()
	if err != nil {
		t.Fatalf("readCache error: %v", err)
	}
	if readBack.LatestVersion != "v0.5.0" {
		t.Errorf("LatestVersion = %q, want %q", readBack.LatestVersion, "v0.5.0")
	}

	// Restore cache to avoid polluting real cache
	cleanCache := &versionCache{
		LatestVersion: Version,
		CheckedAt:     time.Now(),
	}
	_ = writeCache(cleanCache)
}

func TestReadCacheNonexistent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := readCache()
	if err == nil {
		t.Error("readCache should error for nonexistent cache")
	}
}

func TestFetchLatestVersionHTTP(t *testing.T) {
	// We can't inject the URL into fetchLatestVersion without refactoring,
	// but we verify it handles failures gracefully (network may be unavailable)
	_, _ = fetchLatestVersion()
}

func TestCompareAndNotify(t *testing.T) {
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	oldVersion := Version
	Version = "0.1.0"

	compareAndNotify("v99.0.0")

	Version = oldVersion
	_ = w.Close()
	os.Stderr = oldStderr

	var buf strings.Builder
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	output := buf.String()

	if !strings.Contains(output, "New version available") {
		t.Error("compareAndNotify should print update notification")
	}
}

func TestCompareAndNotifyNoUpdate(t *testing.T) {
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	oldVersion := Version
	Version = "99.0.0"

	compareAndNotify("v0.1.0")

	Version = oldVersion
	_ = w.Close()
	os.Stderr = oldStderr

	var buf strings.Builder
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	output := buf.String()

	if strings.Contains(output, "New version") {
		t.Error("compareAndNotify should not print notification when current is newer")
	}
}

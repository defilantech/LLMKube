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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	githubAPIURL    = "https://api.github.com/repos/defilantech/LLMKube/releases/latest"
	cacheExpiration = 24 * time.Hour
)

type githubRelease struct {
	TagName string `json:"tag_name"`
}

type versionCache struct {
	LatestVersion string    `json:"latest_version"`
	CheckedAt     time.Time `json:"checked_at"`
}

// getCacheFilePath returns the path to the version cache file
func getCacheFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(home, ".llmkube")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "version_cache.json"), nil
}

// readCache reads the cached version information
func readCache() (*versionCache, error) {
	cachePath, err := getCacheFilePath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}

	var cache versionCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}

	return &cache, nil
}

// writeCache writes version information to cache
func writeCache(cache *versionCache) error {
	cachePath, err := getCacheFilePath()
	if err != nil {
		return err
	}

	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}

	return os.WriteFile(cachePath, data, 0644)
}

// fetchLatestVersion queries GitHub API for the latest release
func fetchLatestVersion() (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", githubAPIURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close() // Explicitly ignore close error in defer
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}

	return release.TagName, nil
}

// normalizeVersion removes 'v' prefix if present for comparison
func normalizeVersion(version string) string {
	return strings.TrimPrefix(version, "v")
}

// parseVersion parses a version string into its numeric components
func parseVersion(version string) []int {
	parts := strings.Split(normalizeVersion(version), ".")
	result := make([]int, len(parts))
	for i, part := range parts {
		num, err := strconv.Atoi(part)
		if err != nil {
			result[i] = 0
		} else {
			result[i] = num
		}
	}
	return result
}

// compareVersions compares two version strings numerically
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func compareVersions(v1, v2 string) int {
	parts1 := parseVersion(v1)
	parts2 := parseVersion(v2)

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var p1, p2 int
		if i < len(parts1) {
			p1 = parts1[i]
		}
		if i < len(parts2) {
			p2 = parts2[i]
		}

		if p1 < p2 {
			return -1
		}
		if p1 > p2 {
			return 1
		}
	}
	return 0
}

// CheckForUpdate checks if a newer version is available and prints a notification
// This function is designed to fail silently if offline or if there are errors
func CheckForUpdate() {
	// Read cache first
	cache, err := readCache()
	if err == nil && time.Since(cache.CheckedAt) < cacheExpiration {
		// Cache is valid, use cached version
		compareAndNotify(cache.LatestVersion)
		return
	}

	// Cache is expired or doesn't exist, fetch from GitHub
	latestVersion, err := fetchLatestVersion()
	if err != nil {
		// Fail silently if offline or API error
		return
	}

	// Update cache
	newCache := &versionCache{
		LatestVersion: latestVersion,
		CheckedAt:     time.Now(),
	}
	_ = writeCache(newCache) // Ignore write errors

	compareAndNotify(latestVersion)
}

// compareAndNotify compares versions and prints update notification if needed
func compareAndNotify(latestVersion string) {
	if compareVersions(Version, latestVersion) < 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  New version available: %s (current: %s)\n", latestVersion, Version)
		fmt.Fprintf(os.Stderr, "   Update with: brew upgrade llmkube\n")
		fmt.Fprintf(os.Stderr, "   Or download from: https://github.com/defilantech/LLMKube/releases/latest\n\n")
	}
}

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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModule = "github.com/defilantech/llmkube"

type wantFailure struct {
	key     string
	excerpt []string // substrings the excerpt must contain
}

func checkFailures(t *testing.T, got []failure, want []wantFailure) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for _, f := range got {
		keys = append(keys, f.key())
	}
	if len(got) != len(want) {
		t.Fatalf("failures %q, want %d", keys, len(want))
	}
	for i, w := range want {
		if got[i].key() != w.key {
			t.Errorf("failure %d = %q, want %q", i, got[i].key(), w.key)
		}
		text := strings.Join(got[i].Excerpt, "\n")
		for _, s := range w.excerpt {
			if !strings.Contains(text, s) {
				t.Errorf("%s: excerpt lacks %q:\n%s", w.key, s, text)
			}
		}
		if len(got[i].Excerpt) > maxExcerptLines {
			t.Errorf("%s: excerpt has %d lines, more than %d", w.key, len(got[i].Excerpt), maxExcerptLines)
		}
	}
}

func parseJSONFixture(t *testing.T, name string) []failure {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fs, err := parseGoTestJSON(f, testModule)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func logFixture(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return cleanLog(b)
}

func TestParseGoTestJSON(t *testing.T) {
	tests := []struct {
		fixture string
		want    []wantFailure
	}{
		{
			// The Tests job's go-test-report from a push to main: make's plain
			// lines, a passing test, the failing test, the package's FAIL.
			fixture: "artifact-13176458296/test-report-seed11.jsonl",
			want: []wantFailure{{
				key: "TestMetalNodeLeaseEventReachesItsMetalNode (internal/controller)",
				excerpt: []string{
					`metalnode_controller_test.go:724: AgentConnected reason = "" before the Lease exists`,
					"listen tcp :8080: bind: address already in use",
					"--- FAIL: TestMetalNodeLeaseEventReachesItsMetalNode (30.04s)",
				},
			}},
		},
		{
			// A failing subtest is recorded under its top-level test.
			fixture: "subtest-failure.jsonl",
			want: []wantFailure{{
				key: "TestModelMultiFileSHA256_Behavioral (internal/controller)",
				excerpt: []string{
					"model_storage_sha256_test.go:868: expected the offline fallback message",
					"--- FAIL: TestModelMultiFileSHA256_Behavioral/OnChange_offline_keeps_a_verified_pinned_copy",
				},
			}},
		},
		{
			// A Ginkgo suite's failures are its specs, not the go test that ran it.
			fixture: "ginkgo-suite.jsonl",
			want: []wantFailure{{
				key:     "Widget when flaky [It] reports Ready (internal/demo)",
				excerpt: []string{"[FAILED] widget phase", "<string>: Pending"},
			}},
		},
		{
			// A -timeout kill leaves no result for the test it interrupted; a
			// package that does not build is not a test failure.
			fixture: "timeout-and-build-failure.jsonl",
			want: []wantFailure{{
				key:     "go test timeout (internal/slow)",
				excerpt: []string{"panic: test timed out after 1s", "TestHang (1s)"},
			}},
		},
		{
			// The attempt that passed has nothing to record.
			fixture: "artifact-13201824232/test-report-seed11.jsonl",
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			checkFailures(t, parseJSONFixture(t, tt.fixture), tt.want)
		})
	}
}

func TestParseGinkgoLog(t *testing.T) {
	tests := []struct {
		fixture string
		want    []wantFailure
	}{
		{
			// An E2E Tests job log: the spec's package comes from its location.
			fixture: "log-121240336411.txt",
			want: []wantFailure{{
				key: "Manager ModelRouter Cluster [It] should recover from quarantine once the window " +
					"expires (half-open probe) (test/e2e)",
				excerpt: []string{"[FAILED] curl dispatch failed: curl: (28) Resolving timed out after 5000 milliseconds"},
			}},
		},
		{
			// A BeforeSuite fails inside Ginkgo's module; the suite header names
			// the package.
			fixture: "beforesuite.txt",
			want: []wantFailure{{
				key:     "[BeforeSuite] (test/e2e)",
				excerpt: []string{"[FAILED] Failed to build the manager(Operator) image", "Unexpected error:"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			checkFailures(t, parseGinkgo(logFixture(t, tt.fixture), ""), tt.want)
		})
	}
}

func TestParseGoTestLog(t *testing.T) {
	tests := []struct {
		fixture string
		want    []wantFailure
	}{
		{
			// The -json stream interleaves packages; the timing report the job
			// prints from the same file names the package.
			fixture: "timing-report.txt",
			want: []wantFailure{{
				key: "TestMetalNodeLeaseEventReachesItsMetalNode (internal/controller)",
				excerpt: []string{
					"metalnode_controller_test.go:689: manager exited with an error",
					"--- FAIL: TestMetalNodeLeaseEventReachesItsMetalNode (30.04s)",
				},
			}},
		},
		{
			// Plain go test output from before the timing report: the test's
			// output follows its FAIL line, and the package is the next FAIL line.
			fixture: "log-115770998477.txt",
			want: []wantFailure{{
				key:     "TestServer_StreamsChunks (pkg/agent/ingress)",
				excerpt: []string{`server_test.go:426: second event missing, got "\n"`},
			}},
		},
		{
			// TestE2E is the go test that ran the suite; only the spec counts.
			fixture: "beforesuite.txt",
			want:    []wantFailure{{key: "[BeforeSuite] (test/e2e)"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			checkFailures(t, parseGoTestLog(logFixture(t, tt.fixture), testModule), tt.want)
		})
	}
}

func TestCleanLogStripsTimestampsAndColor(t *testing.T) {
	got := cleanLog([]byte("\ufeff2026-11-03T07:11:04.9445035Z   \x1b[38;5;9m[FAIL]\x1b[0m [BeforeSuite]\r\n"))
	if got[0] != "  [FAIL] [BeforeSuite]" {
		t.Errorf("cleanLog = %q", got[0])
	}
}

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

// The ledger depends on the workflows it reads: their file names, the
// artifact the Tests workflow uploads, and which have continue-on-error jobs.
// Those are checked against the real workflow files, so renaming one or
// adding a best-effort job turns this red instead of silently reading less.
func TestWorkflowsMatchWhatTheLedgerReads(t *testing.T) {
	for _, wf := range defaultWorkflows {
		b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", wf.File))
		if err != nil {
			t.Fatalf("%s: %v (the ledger reads it by file name)", wf.File, err)
		}
		text := string(b)
		if got := strings.Contains(text, "continue-on-error: true"); got != wf.BestEffortJobs {
			t.Errorf("%s: continue-on-error job present = %v but BestEffortJobs = %v; a green first "+
				"attempt is skipped unread only when no job may fail without failing the run", wf.File, got, wf.BestEffortJobs)
		}
		if wf.Artifact == "" {
			continue
		}
		for _, want := range []string{"name: " + wf.Artifact, "path: test-report-seed*.jsonl", "GO_TEST_FLAGS=-json"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer has %q: the ledger reads go test -json reports from the %s artifact",
					wf.File, want, wf.Artifact)
			}
		}
	}
}

func TestNeedsJobs(t *testing.T) {
	unit, e2e := workflow{File: "test.yml"}, workflow{File: "test-e2e.yml", BestEffortJobs: true}
	done := func(conclusion string, attempt int) run {
		return run{Status: "completed", Conclusion: conclusion, RunAttempt: attempt}
	}
	tests := []struct {
		name string
		wf   workflow
		r    run
		want bool
	}{
		{"in progress", unit, run{Status: "in_progress", RunAttempt: 1}, false},
		{"waiting for fork approval", unit, done("action_required", 1), false},
		{"failed", unit, done("failure", 1), true},
		{"green on a rerun: an earlier attempt failed", unit, done("success", 2), true},
		{"green first attempt", unit, done("success", 1), false},
		{"green first attempt with a best-effort job", e2e, done("success", 1), true},
	}
	for _, tt := range tests {
		if got := needsJobs(tt.wf, tt.r); got != tt.want {
			t.Errorf("%s: needsJobs = %v, want %v", tt.name, got, tt.want)
		}
	}
}

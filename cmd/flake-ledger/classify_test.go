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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The runs and jobs below are Actions API responses recorded from
// defilantech/LLMKube (testdata/README.md).

func loadRun(t *testing.T, file string, id int64) run {
	t.Helper()
	var page struct {
		WorkflowRuns []run `json:"workflow_runs"`
	}
	readJSON(t, file, &page)
	for _, r := range page.WorkflowRuns {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("run %d not in %s", id, file)
	return run{}
}

func loadJobs(t *testing.T, runID int64) []job {
	t.Helper()
	var page struct {
		Jobs []job `json:"jobs"`
	}
	readJSON(t, "jobs-"+strconv.FormatInt(runID, 10)+".json", &page)
	return page.Jobs
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// stubReader records which job attempts were read and reports one failing
// test for each.
type stubReader struct{ read []int64 }

func (s *stubReader) source() failureSource {
	return func(_ context.Context, _ workflow, _ run, j job) ([]failure, string, error) {
		s.read = append(s.read, j.ID)
		return []failure{{Name: "TestFlaky", Package: "internal/x"}}, "stub", nil
	}
}

func TestOccurrencesForRun(t *testing.T) {
	tests := []struct {
		name       string
		run        run
		jobs       []job
		wantKind   kind
		wantJobs   []int64 // failed job attempts recorded, in order
		wantPass   int
		wantPROnly int
	}{
		{
			// Tests run 41570133041: attempt 1 failed, attempt 2 of the same
			// job passed, and the run's conclusion is success.
			name:     "a failure that passed on a later attempt of the same run is pass-on-rerun",
			run:      loadRun(t, "runs-test.yml.json", 41570133041),
			jobs:     loadJobs(t, 41570133041),
			wantKind: kindRerun,
			wantJobs: []int64{125700832816},
			wantPass: 2,
		},
		{
			name:     "a failure on a push to main is main",
			run:      loadRun(t, "runs-test.yml.json", 41518210793),
			jobs:     loadJobs(t, 41518210793),
			wantKind: kindMain,
			wantJobs: []int64{125527126456},
		},
		{
			// Tests run 39985914548: one PR's own commit, never rerun.
			name:       "a failure only on a PR branch is not recorded or even read",
			run:        loadRun(t, "runs-test.yml.json", 39985914548),
			jobs:       loadJobs(t, 39985914548),
			wantPROnly: 1,
		},
		{
			// E2E Tests run 40215925566: attempt 1 waited for fork approval and
			// has no jobs; attempt 2's default-storage job failed and attempt 3's
			// passed, while the other jobs were carried over.
			name:     "an e2e job that failed on attempt 2 and passed on attempt 3 is pass-on-rerun",
			run:      loadRun(t, "runs-test-e2e.yml.json", 40215925566),
			jobs:     loadJobs(t, 40215925566),
			wantKind: kindRerun,
			wantJobs: []int64{121240336411},
			wantPass: 3,
		},
		{
			// E2E Tests run 40604254204: the rerun failed the same way.
			name:       "a PR failure whose rerun also failed is PR-only, both attempts",
			run:        loadRun(t, "runs-test-e2e.yml.json", 40604254204),
			jobs:       loadJobs(t, 40604254204),
			wantPROnly: 2,
		},
		{
			name: "a failure in the merge queue is merge-queue",
			run: func() run {
				r := loadRun(t, "runs-test.yml.json", 39985914548)
				r.Event, r.HeadBranch = "merge_group", "gh-readonly-queue/main/pr-2379-2116b4d66"
				return r
			}(),
			jobs:     loadJobs(t, 39985914548),
			wantKind: kindMergeQueue,
			wantJobs: []int64{120524920169},
		},
		{
			name: "a main failure that passed on a rerun stays main",
			run: func() run {
				r := loadRun(t, "runs-test.yml.json", 41570133041)
				r.Event, r.HeadBranch = "push", "main"
				return r
			}(),
			jobs:     loadJobs(t, 41570133041),
			wantKind: kindMain,
			wantJobs: []int64{125700832816},
			wantPass: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubReader{}
			var st stats
			occ, err := occurrencesForRun(context.Background(), defaultWorkflows[0], tt.run, tt.jobs, "main",
				stub.source(), &st)
			if err != nil {
				t.Fatal(err)
			}
			var got []int64
			for _, o := range occ {
				got = append(got, o.Job.ID)
				if o.Kind != tt.wantKind || o.PassedOn != tt.wantPass {
					t.Errorf("job %d: kind %s passedOn %d, want %s passedOn %d",
						o.Job.ID, o.Kind, o.PassedOn, tt.wantKind, tt.wantPass)
				}
			}
			if !equalIDs(got, tt.wantJobs) {
				t.Errorf("recorded job attempts %v, want %v", got, tt.wantJobs)
			}
			// Only recorded attempts are read: a PR-only failure costs no
			// artifact or log download.
			if !equalIDs(stub.read, tt.wantJobs) {
				t.Errorf("read job attempts %v, want %v", stub.read, tt.wantJobs)
			}
			if st.PROnlyJobs != tt.wantPROnly {
				t.Errorf("PR-only failed jobs = %d, want %d", st.PROnlyJobs, tt.wantPROnly)
			}
		})
	}
}

func TestClassifyMatchesTheSameJobOnALaterAttempt(t *testing.T) {
	pr := run{Event: "pull_request", HeadBranch: "feat/x"}
	failed := job{ID: 1, Name: "Unit and envtest", RunAttempt: 2, Conclusion: "failure"}
	tests := []struct {
		name string
		jobs []job
		want kind
	}{
		{"another job passing later is not a rerun of this one",
			[]job{failed, {ID: 2, Name: "Lint and static checks", RunAttempt: 3, Conclusion: "success"}}, kindPROnly},
		{"the same job passing on an earlier attempt is not a rerun",
			[]job{{ID: 3, Name: "Unit and envtest", RunAttempt: 1, Conclusion: "success"}, failed}, kindPROnly},
		{"a later attempt that was cancelled is not a pass",
			[]job{failed, {ID: 4, Name: "Unit and envtest", RunAttempt: 3, Conclusion: "cancelled"}}, kindPROnly},
		{"the same job passing on a later attempt is a rerun",
			[]job{failed, {ID: 5, Name: "Unit and envtest", RunAttempt: 3, Conclusion: "success"}}, kindRerun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := classify(pr, failed, tt.jobs, "main"); got != tt.want {
				t.Errorf("classify = %s, want %s", got, tt.want)
			}
		})
	}
	// A push to a branch other than main is a branch's own commit.
	other := run{Event: "push", HeadBranch: "release-0.10"}
	if got, _ := classify(other, failed, []job{failed}, "main"); got != kindPROnly {
		t.Errorf("push to release-0.10 = %s, want %s", got, kindPROnly)
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

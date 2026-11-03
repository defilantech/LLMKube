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
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// workflow is a test workflow the ledger reads. Workflows are named by file,
// never by job name: job names change ("Run on Ubuntu" became "Unit and
// envtest") and several workflows share them.
type workflow struct {
	File string
	// Artifact is the go test -json report the workflow uploads per attempt
	// (#2141). Empty for workflows that only have job logs.
	Artifact string
	// GoTests means the job log may carry plain go test failures; otherwise
	// only Ginkgo specs are read from it (e2e runs one go test, TestE2E,
	// whose failures are its specs).
	GoTests bool
	// BestEffortJobs: some job has continue-on-error, so a run that passed
	// on its first attempt can still hold a failed job. Without it such a run
	// is skipped unread, which halves the API calls for a long --since. Set
	// it when a workflow gains a continue-on-error job.
	BestEffortJobs bool
}

var defaultWorkflows = []workflow{
	{File: "test.yml", Artifact: "go-test-report", GoTests: true},
	{File: "test-e2e.yml", BestEffortJobs: true},
}

type run struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Event          string    `json:"event"`
	HeadBranch     string    `json:"head_branch"`
	HeadSHA        string    `json:"head_sha"`
	RunAttempt     int       `json:"run_attempt"`
	Status         string    `json:"status"`
	Conclusion     string    `json:"conclusion"`
	CreatedAt      time.Time `json:"created_at"`
	HTMLURL        string    `json:"html_url"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

type job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	RunAttempt  int       `json:"run_attempt"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	HTMLURL     string    `json:"html_url"`
}

type artifact struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Expired   bool      `json:"expired"`
	CreatedAt time.Time `json:"created_at"`
}

// kind is where a failure happened, which decides whether it marks a flake.
type kind string

const (
	// kindMain: a push to the main branch, where every commit already
	// passed review and CI.
	kindMain kind = "main"
	// kindMergeQueue: a merge_group run, the PR on top of main. The PR's own
	// CI passed, so a failure here is not the PR's own commits failing.
	kindMergeQueue kind = "merge-queue"
	// kindRerun: failed, then passed on a later attempt of the same run,
	// which is the same commit.
	kindRerun kind = "pass-on-rerun"
	// kindPROnly: failed on a branch's own commit and never passed on a
	// rerun. That is CI doing its job, so it is not recorded.
	kindPROnly kind = "pr-only"
)

func (k kind) flaky() bool { return k != kindPROnly }

// classify decides the kind of a failed job attempt from its run and every
// attempt of the run's jobs. passedOn is the later attempt where the same
// job succeeded, 0 when none did.
func classify(r run, failed job, jobs []job, mainBranch string) (k kind, passedOn int) {
	for _, j := range jobs {
		if j.Name == failed.Name && j.RunAttempt > failed.RunAttempt && j.Conclusion == "success" &&
			(passedOn == 0 || j.RunAttempt < passedOn) {
			passedOn = j.RunAttempt
		}
	}
	switch {
	case r.Event == "push" && r.HeadBranch == mainBranch:
		return kindMain, passedOn
	case r.Event == "merge_group":
		return kindMergeQueue, passedOn
	case passedOn > 0:
		return kindRerun, passedOn
	}
	return kindPROnly, 0
}

// occurrence is one test failing in one job attempt, classified.
type occurrence struct {
	failure
	Kind     kind
	PassedOn int
	Run      run
	Job      job
	Source   string
}

// stats counts what the collection saw and skipped, for the summary.
type stats struct {
	Runs          map[string]int
	FailedJobs    int
	NoTestFailure int
	Unreadable    []string
	PROnlyJobs    int
}

// failureSource reads the failing tests of one failed job attempt.
type failureSource func(ctx context.Context, wf workflow, r run, j job) (failures []failure, source string, err error)

// occurrencesForRun classifies every failed job attempt of a run and the
// tests that failed in it. Every attempt is read, not only the latest: a run's
// conclusion is its last attempt's, so a failure that passed on a rerun is
// invisible to anything that looks only at the run.
func occurrencesForRun(ctx context.Context, wf workflow, r run, jobs []job, mainBranch string,
	read failureSource, st *stats) ([]occurrence, error) {
	sort.SliceStable(jobs, func(a, b int) bool {
		if jobs[a].RunAttempt != jobs[b].RunAttempt {
			return jobs[a].RunAttempt < jobs[b].RunAttempt
		}
		return jobs[a].ID < jobs[b].ID
	})
	var out []occurrence
	for _, j := range jobs {
		if j.Conclusion != "failure" {
			continue
		}
		st.FailedJobs++
		k, passedOn := classify(r, j, jobs, mainBranch)
		// A PR-only failure is not read at all: it is never recorded, and
		// skipping it keeps a long --since inside the API rate limit.
		if !k.flaky() {
			st.PROnlyJobs++
			continue
		}
		failures, source, err := read(ctx, wf, r, j)
		if errors.Is(err, errGone) {
			st.Unreadable = append(st.Unreadable, fmt.Sprintf("job %d (%s run %d attempt %d): %v",
				j.ID, r.Name, r.ID, j.RunAttempt, err))
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(failures) == 0 {
			st.NoTestFailure++
			continue
		}
		seen := map[string]bool{}
		for _, f := range failures {
			if seen[f.key()] {
				continue
			}
			seen[f.key()] = true
			out = append(out, occurrence{failure: f, Kind: k, PassedOn: passedOn, Run: r, Job: j, Source: source})
		}
	}
	return out, nil
}

// collector reads runs, jobs, artifacts and logs from the Actions API.
type collector struct {
	gh         *gitHub
	repo       string
	module     string
	mainBranch string
	// workers bounds the runs read at once. A 14-day window is about 600
	// runs; read one at a time that takes over ten minutes.
	workers int
}

func (s *stats) add(o stats) {
	s.FailedJobs += o.FailedJobs
	s.NoTestFailure += o.NoTestFailure
	s.PROnlyJobs += o.PROnlyJobs
	s.Unreadable = append(s.Unreadable, o.Unreadable...)
}

// collect returns the flaky occurrences in runs of wfs created at or after since.
func (c *collector) collect(ctx context.Context, wfs []workflow, since time.Time) ([]occurrence, stats, error) {
	st := stats{Runs: map[string]int{}}
	type task struct {
		wf workflow
		r  run
	}
	var tasks []task
	for _, wf := range wfs {
		runs, err := c.runs(ctx, wf, since)
		if err != nil {
			return nil, st, err
		}
		for _, r := range runs {
			st.Runs[wf.File]++
			if needsJobs(wf, r) {
				tasks = append(tasks, task{wf, r})
			}
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		out      []occurrence
		firstErr error
	)
	ch := make(chan task)
	for range max(1, c.workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				var local stats
				jobs, err := c.jobs(ctx, t.r.ID)
				var occ []occurrence
				if err == nil {
					occ, err = occurrencesForRun(ctx, t.wf, t.r, jobs, c.mainBranch, c.reader(), &local)
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("%s run %d: %w", t.r.Name, t.r.ID, err)
					cancel()
				}
				out = append(out, occ...)
				st.add(local)
				mu.Unlock()
			}
		}()
	}
feed:
	for _, t := range tasks {
		select {
		case ch <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	sort.Strings(st.Unreadable)
	return out, st, firstErr
}

// needsJobs reports whether a run can hold a failed job attempt worth reading.
func needsJobs(wf workflow, r run) bool {
	switch {
	// A run still going may yet be rerun or finish; the next ledger run's
	// overlapping window reads it once it is done.
	case r.Status != "completed":
		return false
	// A fork's run waiting for approval, or skipped, ran no jobs.
	case r.Conclusion == "action_required" || r.Conclusion == "skipped":
		return false
	case r.Conclusion == "success" && r.RunAttempt == 1:
		return wf.BestEffortJobs
	}
	return true
}

func (c *collector) runs(ctx context.Context, wf workflow, since time.Time) ([]run, error) {
	q := url.Values{}
	q.Set("created", ">="+since.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	q.Set("exclude_pull_requests", "true")
	p := fmt.Sprintf("repos/%s/actions/workflows/%s/runs?%s", c.repo, url.PathEscape(wf.File), q.Encode())
	var runs []run
	err := c.gh.pages(ctx, p, func(b []byte) error {
		var page struct {
			TotalCount   int   `json:"total_count"`
			WorkflowRuns []run `json:"workflow_runs"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		// The runs search stops at 1000 results.
		if page.TotalCount > 1000 {
			return fmt.Errorf("%s has %d runs since %s, more than the API returns; use a shorter --since",
				wf.File, page.TotalCount, since.Format(time.RFC3339))
		}
		runs = append(runs, page.WorkflowRuns...)
		return nil
	})
	return runs, err
}

// jobs returns the jobs of every attempt of a run (filter=all), so a failed
// attempt that a rerun replaced is still there.
func (c *collector) jobs(ctx context.Context, runID int64) ([]job, error) {
	var jobs []job
	p := fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=all&per_page=100", c.repo, runID)
	err := c.gh.pages(ctx, p, func(b []byte) error {
		var page struct {
			Jobs []job `json:"jobs"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		jobs = append(jobs, page.Jobs...)
		return nil
	})
	return jobs, err
}

// reader returns a failureSource for the jobs of one run. It reads a failed
// job attempt from the attempt's go-test-report when the workflow uploads one
// and it has not expired, and from the job log otherwise.
func (c *collector) reader() failureSource {
	var arts []artifact
	listed := false
	return func(ctx context.Context, wf workflow, r run, j job) ([]failure, string, error) {
		if wf.Artifact != "" {
			if !listed {
				var err error
				if arts, err = c.listArtifacts(ctx, wf.Artifact, r.ID); err != nil {
					return nil, "", err
				}
				listed = true
			}
			if a := attemptArtifact(arts, wf.Artifact, j); a != nil {
				fs, err := c.artifactFailures(ctx, a.ID)
				if err == nil {
					return fs, wf.Artifact + " artifact", nil
				}
				if !isGone(err) {
					return nil, "", err
				}
			}
		}
		raw, _, err := c.gh.get(ctx, fmt.Sprintf("repos/%s/actions/jobs/%d/logs", c.repo, j.ID))
		if err != nil {
			return nil, "", err
		}
		lines := cleanLog(raw)
		if wf.GoTests {
			return parseGoTestLog(lines, c.module), "job log", nil
		}
		return parseGinkgo(lines, ""), "job log", nil
	}
}

func (c *collector) listArtifacts(ctx context.Context, name string, runID int64) ([]artifact, error) {
	q := url.Values{"name": {name}, "per_page": {"100"}}
	var arts []artifact
	err := c.gh.pages(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/artifacts?%s", c.repo, runID, q.Encode()),
		func(b []byte) error {
			var page struct {
				Artifacts []artifact `json:"artifacts"`
			}
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			arts = append(arts, page.Artifacts...)
			return nil
		})
	return arts, err
}

// attemptArtifact finds the artifact one job attempt uploaded. Artifacts carry
// no attempt number, and every attempt of a run uploads one under the same
// name, so the attempt is the one whose job was running when it was created.
func attemptArtifact(arts []artifact, name string, j job) *artifact {
	var best *artifact
	for i := range arts {
		a := &arts[i]
		if a.Name != name || a.Expired || a.CreatedAt.Before(j.StartedAt) || a.CreatedAt.After(j.CompletedAt) {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			best = a
		}
	}
	return best
}

func (c *collector) artifactFailures(ctx context.Context, id int64) ([]failure, error) {
	raw, _, err := c.gh.get(ctx, fmt.Sprintf("repos/%s/actions/artifacts/%d/zip", c.repo, id))
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("artifact %d: %w", id, err)
	}
	var out []failure
	names := make([]string, 0, len(zr.File))
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".jsonl") {
			names = append(names, f.Name)
			files[f.Name] = f
		}
	}
	sort.Strings(names)
	for _, n := range names {
		rc, err := files[n].Open()
		if err != nil {
			return nil, fmt.Errorf("artifact %d %s: %w", id, path.Base(n), err)
		}
		fs, err := parseGoTestJSON(rc, c.module)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("artifact %d %s: %w", id, path.Base(n), err)
		}
		out = append(out, fs...)
	}
	return out, nil
}

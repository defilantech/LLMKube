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

// Command flake-ledger records flaky tests as GitHub issues (#2378).
//
// It reads the Tests and E2E Tests workflow runs created since --since, every
// attempt of every job, and the tests each failed job attempt failed: from the
// attempt's go-test-report artifact (go test -json) when the workflow uploads
// one and it has not expired, from the job log otherwise (the Go timing report
// or `--- FAIL` lines, and Ginkgo's failure summary).
//
// A failure is classified as main (a push to main), merge-queue (a
// merge_group run), pass-on-rerun (a later attempt of the same job in the same
// run passed), or PR-only. PR-only failures are a branch's own commits failing,
// which is CI doing its job, and are not recorded. Every other failure is
// recorded on the test's one open issue, titled "[FLAKY] <test> (<package>)"
// and labelled flaky-test, one comment per occurrence. Each comment starts
// with a marker naming the job, so a rerun of the ledger over the same window
// records nothing twice.
//
// Usage:
//
//	hack/flake-ledger.sh --since 7d --dry-run
//	go run ./cmd/flake-ledger --since 2026-10-20 --dry-run -v
//
// It needs GH_TOKEN (or GITHUB_TOKEN) with actions:read, and issues:write
// unless --dry-run, which only reads.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := runLedger(context.Background(), os.Args[1:], os.Stdout, os.Getenv, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "flake-ledger:", err)
		os.Exit(1)
	}
}

type config struct {
	repo       string
	api        string
	token      string
	since      time.Time
	dryRun     bool
	verbose    bool
	label      string
	mainBranch string
	module     string
	maxNew     int
	workflows  []workflow
}

func parseFlags(args []string, getenv func(string) string, now time.Time) (config, error) {
	fs := flag.NewFlagSet("flake-ledger", flag.ContinueOnError)
	c := config{workflows: defaultWorkflows}
	since := fs.String("since", "3d", "read runs created since this long ago (3d, 36h) or this date (2026-10-20)")
	fs.StringVar(&c.repo, "repo", orDefault(getenv("GITHUB_REPOSITORY"), "defilantech/LLMKube"), "owner/name")
	fs.StringVar(&c.api, "api", orDefault(getenv("GITHUB_API_URL"), "https://api.github.com"), "REST API base URL")
	fs.BoolVar(&c.dryRun, "dry-run", false, "print what would be filed; only read the API")
	fs.BoolVar(&c.verbose, "v", false, "print each occurrence's comment body")
	fs.StringVar(&c.label, "label", "flaky-test", "label on every flaky-test issue")
	fs.StringVar(&c.mainBranch, "main-branch", "main", "branch whose pushes count as main")
	fs.StringVar(&c.module, "module", "github.com/defilantech/llmkube", "Go module path, trimmed from package names")
	fs.IntVar(&c.maxNew, "max-new-issues", 10, "refuse to open more issues than this in one run")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	t, err := parseSince(*since, now)
	if err != nil {
		return c, err
	}
	c.since = t
	c.token = orDefault(getenv("GH_TOKEN"), getenv("GITHUB_TOKEN"))
	if c.token == "" {
		return c, fmt.Errorf("set GH_TOKEN or GITHUB_TOKEN")
	}
	return c, nil
}

// parseSince reads a number of days or hours back from now (7d, 36h), a date
// (2026-10-20, midnight UTC) or an RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if d, err := strconv.Atoi(n); err == nil && d > 0 {
			return now.Add(-time.Duration(d) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want 3d, 36h, 2026-10-20 or an RFC 3339 time", s)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func runLedger(ctx context.Context, args []string, out io.Writer, getenv func(string) string, now time.Time) error {
	c, err := parseFlags(args, getenv, now)
	if err != nil {
		return err
	}
	gh := &gitHub{
		api:       c.api,
		token:     c.token,
		http:      &http.Client{Timeout: 2 * time.Minute},
		retryWait: 5 * time.Second,
		readOnly:  c.dryRun,
	}
	mode := ""
	if c.dryRun {
		mode = " (dry run: read only)"
	}
	_, _ = fmt.Fprintf(out, "flake ledger for %s, runs created since %s%s\n",
		c.repo, c.since.UTC().Format(time.RFC3339), mode)

	col := &collector{gh: gh, repo: c.repo, module: c.module, mainBranch: c.mainBranch, workers: 6}
	occs, st, err := col.collect(ctx, c.workflows, c.since)
	if err != nil {
		return err
	}
	for _, wf := range c.workflows {
		_, _ = fmt.Fprintf(out, "%s: %d runs\n", wf.File, st.Runs[wf.File])
	}
	_, _ = fmt.Fprintf(out, "%d failed job attempts: %d PR-only (not recorded), %d with no failing test "+
		"(build, lint, setup or a step outside the tests), %d unreadable\n",
		st.FailedJobs, st.PROnlyJobs, st.NoTestFailure, len(st.Unreadable))
	for _, u := range st.Unreadable {
		_, _ = fmt.Fprintln(out, "  unreadable: "+u)
	}
	_, _ = fmt.Fprintln(out)

	l := &ledger{gh: gh, repo: c.repo, label: c.label, maxNew: c.maxNew, dryRun: c.dryRun, verbose: c.verbose, out: out}
	if err := l.record(ctx, occs); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "\n%d API reads, %d writes\n", gh.reads.Load(), gh.writes.Load())
	return nil
}

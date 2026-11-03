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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeRepo = "defilantech/LLMKube"

type fakeIssue struct {
	Number   int
	Title    string
	Body     string
	State    string
	ClosedAt *time.Time
	Labels   []string
	IsPR     bool
	Comments []string
}

// fakeGitHub serves the recorded Actions API responses in testdata and keeps
// issues, comments and labels in memory. Lists are served two items to a page
// so the ledger has to follow Link headers.
type fakeGitHub struct {
	t      *testing.T
	mu     sync.Mutex
	issues []*fakeIssue
	labels map[string]bool
	writes []string
	reads  []string
	srv    *httptest.Server
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, labels: map[string]bool{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

var (
	runsPath      = regexp.MustCompile(`^/repos/` + fakeRepo + `/actions/workflows/([^/]+)/runs$`)
	jobsPath      = regexp.MustCompile(`^/repos/` + fakeRepo + `/actions/runs/(\d+)/jobs$`)
	artifactsPath = regexp.MustCompile(`^/repos/` + fakeRepo + `/actions/runs/(\d+)/artifacts$`)
	zipPath       = regexp.MustCompile(`^/repos/` + fakeRepo + `/actions/artifacts/(\d+)/zip$`)
	logsPath      = regexp.MustCompile(`^/repos/` + fakeRepo + `/actions/jobs/(\d+)/logs$`)
	issuesPath    = regexp.MustCompile(`^/repos/` + fakeRepo + `/issues$`)
	commentsPath  = regexp.MustCompile(`^/repos/` + fakeRepo + `/issues/(\d+)/comments$`)
	labelPath     = regexp.MustCompile(`^/repos/` + fakeRepo + `/labels(?:/([^/]+))?$`)
)

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" && !strings.HasPrefix(r.URL.Path, "/blob/") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	call := r.Method + " " + r.URL.Path
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, call)
	} else {
		f.writes = append(f.writes, call)
	}
	if f.serveActions(w, r) || f.serveIssues(w, r) {
		return
	}
	f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
	http.NotFound(w, r)
}

// serveActions answers the read-only Actions API from the recorded fixtures.
func (f *fakeGitHub) serveActions(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	switch {
	case runsPath.MatchString(p):
		if !strings.HasPrefix(r.URL.Query().Get("created"), ">=") {
			http.Error(w, "runs listed without a created filter", http.StatusBadRequest)
			return true
		}
		f.file(w, "runs-"+runsPath.FindStringSubmatch(p)[1]+".json")
	case jobsPath.MatchString(p):
		if r.URL.Query().Get("filter") != "all" {
			http.Error(w, "jobs listed without filter=all only show the latest attempt", http.StatusBadRequest)
			return true
		}
		f.file(w, "jobs-"+jobsPath.FindStringSubmatch(p)[1]+".json")
	case artifactsPath.MatchString(p):
		f.file(w, "artifacts-"+artifactsPath.FindStringSubmatch(p)[1]+".json")
	case zipPath.MatchString(p):
		// The API answers with a redirect to blob storage.
		w.Header().Set("Location", "/blob/artifact/"+zipPath.FindStringSubmatch(p)[1])
		w.WriteHeader(http.StatusFound)
	case logsPath.MatchString(p):
		w.Header().Set("Location", "/blob/log/"+logsPath.FindStringSubmatch(p)[1])
		w.WriteHeader(http.StatusFound)
	case strings.HasPrefix(p, "/blob/artifact/"):
		f.zip(w, strings.TrimPrefix(p, "/blob/artifact/"))
	case strings.HasPrefix(p, "/blob/log/"):
		f.file(w, "log-"+strings.TrimPrefix(p, "/blob/log/")+".txt")
	default:
		return false
	}
	return true
}

// serveIssues keeps issues, comments and labels in memory.
func (f *fakeGitHub) serveIssues(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	get, post := r.Method == http.MethodGet, r.Method == http.MethodPost
	switch {
	case get && issuesPath.MatchString(p):
		label, state := r.URL.Query().Get("labels"), r.URL.Query().Get("state")
		var out []map[string]any
		for _, is := range f.issues {
			if hasLabel(is, label) && (state == "all" || state == is.State) {
				out = append(out, issueJSON(is))
			}
		}
		f.page(w, r, out)
	case get && commentsPath.MatchString(p):
		is := f.issue(commentsPath.FindStringSubmatch(p)[1])
		if is == nil {
			http.NotFound(w, r)
			return true
		}
		out := make([]map[string]any, 0, len(is.Comments))
		for _, c := range is.Comments {
			out = append(out, map[string]any{"body": c})
		}
		f.page(w, r, out)
	case get && labelPath.MatchString(p):
		if !f.labels[labelPath.FindStringSubmatch(p)[1]] {
			http.NotFound(w, r)
			return true
		}
		_, _ = w.Write([]byte(`{}`))
	case post && labelPath.MatchString(p):
		var in struct{ Name string }
		f.decode(r, &in)
		f.labels[in.Name] = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	case post && issuesPath.MatchString(p):
		var in struct {
			Title, Body string
			Labels      []string
		}
		f.decode(r, &in)
		is := &fakeIssue{Number: 2400 + len(f.issues), Title: in.Title, Body: in.Body, State: "open", Labels: in.Labels}
		f.issues = append(f.issues, is)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(issueJSON(is))
	case post && commentsPath.MatchString(p):
		is := f.issue(commentsPath.FindStringSubmatch(p)[1])
		var in struct{ Body string }
		f.decode(r, &in)
		is.Comments = append(is.Comments, in.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	default:
		return false
	}
	return true
}

func (f *fakeGitHub) file(w http.ResponseWriter, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	_, _ = w.Write(b) //nolint:gosec // G705: serves a recorded fixture to the test's own client
}

func (f *fakeGitHub) zip(w http.ResponseWriter, id string) {
	files, _ := filepath.Glob(filepath.Join("testdata", "artifact-"+id, "*.jsonl"))
	if len(files) == 0 {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			f.t.Fatal(err)
		}
		fw, _ := zw.Create(filepath.Base(name))
		_, _ = fw.Write(b)
	}
	_ = zw.Close()
	_, _ = w.Write(buf.Bytes())
}

func (f *fakeGitHub) page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	const size = 2
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	n = max(n, 1)
	start, end := min((n-1)*size, len(items)), min(n*size, len(items))
	if end < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(n+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.srv.URL, r.URL.Path, q.Encode()))
	}
	page := items[start:end]
	if page == nil {
		page = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (f *fakeGitHub) issue(n string) *fakeIssue {
	for _, is := range f.issues {
		if strconv.Itoa(is.Number) == n {
			return is
		}
	}
	return nil
}

func (f *fakeGitHub) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		f.t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
	}
}

func hasLabel(is *fakeIssue, label string) bool {
	for _, l := range is.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func issueJSON(is *fakeIssue) map[string]any {
	m := map[string]any{
		"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State,
		"closed_at": is.ClosedAt, "html_url": fmt.Sprintf("https://github.com/%s/issues/%d", fakeRepo, is.Number),
	}
	if is.IsPR {
		m["pull_request"] = map[string]any{"url": "x"}
	}
	return m
}

// byTitle returns the open issue with this title.
func (f *fakeGitHub) byTitle(title string) *fakeIssue {
	for _, is := range f.issues {
		if is.Title == title && !is.IsPR && is.State == "open" {
			return is
		}
	}
	return nil
}

// runAgainst runs the ledger against the fake over every recorded run.
func runAgainst(t *testing.T, f *fakeGitHub, extra ...string) (string, error) {
	t.Helper()
	env := map[string]string{"GH_TOKEN": "test-token", "GITHUB_API_URL": f.srv.URL, "GITHUB_REPOSITORY": fakeRepo}
	var out bytes.Buffer
	args := append([]string{"--since", "2026-10-01"}, extra...)
	err := runLedger(context.Background(), args, &out, func(k string) string { return env[k] },
		time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	return out.String(), err
}

const (
	leaseTitle   = "[FLAKY] TestMetalNodeLeaseEventReachesItsMetalNode (internal/controller)"
	streamsTitle = "[FLAKY] TestServer_StreamsChunks (pkg/agent/ingress)"
	e2eTitle     = "[FLAKY] Manager ModelRouter Cluster [It] should recover from quarantine once the window " +
		"expires (half-open probe) (test/e2e)"
	// Job attempts in the recorded runs.
	mainLeaseJob  = 125527126456 // push to main, 2026-11-02
	rerunLeaseJob = 125700832816 // dependabot PR, attempt 1 failed, attempt 2 passed
)

// The PR-only failures in the fixtures: Tests run 39985914548 and E2E Tests
// run 40604254204.
var prOnlyNames = []string{"TestModelMultiFileSHA256_Behavioral", "tight rule.timeout"}

func markers(is *fakeIssue) []string {
	var out []string
	for _, c := range is.Comments {
		if m := recordedMarker.FindStringSubmatch(c); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

func TestLedgerFilesOneIssuePerFlakyTestAndNothingTwice(t *testing.T) {
	f := newFakeGitHub(t)
	out, err := runAgainst(t, f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if len(f.issues) != 3 {
		titles := make([]string, 0, len(f.issues))
		for _, is := range f.issues {
			titles = append(titles, is.Title)
		}
		t.Fatalf("opened %d issues %q, want 3\n%s", len(f.issues), titles, out)
	}
	want := map[string][]string{
		// The main failure and the pass-on-rerun, oldest first.
		leaseTitle: {strconv.Itoa(mainLeaseJob), strconv.Itoa(rerunLeaseJob)},
		// Read from the job log: the run predates the go-test-report artifact.
		streamsTitle: {"115770998477"},
		// Failed on attempt 2, passed on attempt 3.
		e2eTitle: {"121240336411"},
	}
	for title, jobs := range want {
		is := f.byTitle(title)
		if is == nil {
			t.Errorf("no issue %q", title)
			continue
		}
		if !hasLabel(is, "flaky-test") {
			t.Errorf("%s: labels %v", title, is.Labels)
		}
		if got := markers(is); strings.Join(got, ",") != strings.Join(jobs, ",") {
			t.Errorf("%s: recorded jobs %v, want %v", title, got, jobs)
		}
	}
	lease := f.byTitle(leaseTitle)
	if !strings.Contains(lease.Body, keyMarker("TestMetalNodeLeaseEventReachesItsMetalNode (internal/controller)")) {
		t.Errorf("issue body lacks its key marker:\n%s", lease.Body)
	}
	for _, s := range []string{"**main**", "listen tcp :8080: bind: address already in use", "go-test-report artifact"} {
		if !strings.Contains(lease.Comments[0], s) {
			t.Errorf("main comment lacks %q:\n%s", s, lease.Comments[0])
		}
	}
	for _, s := range []string{"**pass-on-rerun**: failed on attempt 1, then passed on attempt 2",
		"dependabot/go_modules/github.com/prometheus/common-0.73.1"} {
		if !strings.Contains(lease.Comments[1], s) {
			t.Errorf("rerun comment lacks %q:\n%s", s, lease.Comments[1])
		}
	}
	if !f.labels["flaky-test"] {
		t.Error("the flaky-test label was not created")
	}

	// A PR-only failure gets no issue, no mention, and is never downloaded.
	for _, is := range f.issues {
		for _, name := range prOnlyNames {
			if strings.Contains(is.Title+is.Body+strings.Join(is.Comments, ""), name) {
				t.Errorf("PR-only failure %q recorded on %q", name, is.Title)
			}
		}
	}
	for _, r := range f.reads {
		for _, id := range []string{"39985914548/artifacts", "120524920169", "122539692586", "122548374018"} {
			if strings.Contains(r, id) {
				t.Errorf("read a PR-only failure: %s", r)
			}
		}
	}

	// The same window again: everything is already recorded.
	before := len(f.writes)
	out, err = runAgainst(t, f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if extra := f.writes[before:]; len(extra) != 0 {
		t.Errorf("second run wrote %v\n%s", extra, out)
	}
	if !strings.Contains(out, leaseTitle+": up to date on #") {
		t.Errorf("second run output:\n%s", out)
	}
}

func TestLedgerCommentsOnlyNewOccurrencesOnTheOpenIssue(t *testing.T) {
	f := newFakeGitHub(t)
	f.labels["flaky-test"] = true
	// A PR with the same title and label is not the test's issue.
	f.issues = append(f.issues, &fakeIssue{Number: 2300, Title: leaseTitle, State: "open",
		Labels: []string{"flaky-test"}, IsPR: true})
	// The issue someone retitled, found by its key marker, already holding
	// the main occurrence and some discussion.
	f.issues = append(f.issues, &fakeIssue{Number: 2377, Title: "[FLAKY] metrics port collision", State: "open",
		Labels: []string{"flaky-test"},
		Body:   keyMarker("TestMetalNodeLeaseEventReachesItsMetalNode (internal/controller)") + "\n...",
		Comments: []string{
			"Looking into this.",
			"<!-- flake-ledger job=" + strconv.Itoa(mainLeaseJob) + " -->\n**main**: ...",
			"Quoting a marker mid-comment does not record anything: <!-- flake-ledger job=" +
				strconv.Itoa(rerunLeaseJob) + " -->",
		}})
	out, err := runAgainst(t, f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.byTitle(leaseTitle) != nil {
		t.Errorf("opened a second issue for the test\n%s", out)
	}
	is := f.issue("2377")
	if got := markers(is); strings.Join(got, ",") != fmt.Sprintf("%d,%d", mainLeaseJob, rerunLeaseJob) {
		t.Errorf("recorded jobs %v, want the main one once and the rerun added\n%s", got, out)
	}
	if !strings.Contains(out, "comment on #2377 with 1 new occurrence(s) (1 already recorded)") {
		t.Errorf("output:\n%s", out)
	}
	for _, w := range f.writes {
		if strings.HasSuffix(w, "/labels") {
			t.Errorf("created the label again: %s", w)
		}
	}
}

func TestLedgerOpensANewIssueOnlyForFailuresAfterTheLastWasClosed(t *testing.T) {
	f := newFakeGitHub(t)
	f.labels["flaky-test"] = true
	// Closed between the main failure (11-02 20:19) and the rerun (11-03 03:06).
	closed := time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	f.issues = append(f.issues, &fakeIssue{Number: 2350, Title: leaseTitle, State: "closed", ClosedAt: &closed,
		Labels: []string{"flaky-test"}})
	out, err := runAgainst(t, f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	is := f.byTitle(leaseTitle)
	if is == nil {
		t.Fatalf("no new issue for the failure after the close\n%s", out)
	}
	if c := f.issue("2350").Comments; len(c) != 0 {
		t.Errorf("commented on the closed issue: %q", c)
	}
	if got := markers(is); strings.Join(got, ",") != strconv.Itoa(rerunLeaseJob) {
		t.Errorf("recorded jobs %v, want only the one after the close", got)
	}
	if !strings.Contains(is.Body, "Previously tracked in #2350.") {
		t.Errorf("new issue does not point at the closed one:\n%s", is.Body)
	}
}

func TestLedgerDryRunOnlyReads(t *testing.T) {
	f := newFakeGitHub(t)
	out, err := runAgainst(t, f, "--dry-run", "-v")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.writes) != 0 {
		t.Errorf("dry run wrote %v", f.writes)
	}
	for _, title := range []string{leaseTitle, streamsTitle, e2eTitle} {
		if !strings.Contains(out, title+": would open a new issue") {
			t.Errorf("dry run does not list %q:\n%s", title, out)
		}
	}
	for _, name := range prOnlyNames {
		if strings.Contains(out, name) {
			t.Errorf("dry run lists PR-only failure %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "| "+jobMarker(rerunLeaseJob)) {
		t.Errorf("-v does not print the comment bodies:\n%s", out)
	}
	if !strings.Contains(out, "3 PR-only (not recorded)") {
		t.Errorf("summary does not count the PR-only failed jobs:\n%s", out)
	}
}

func TestLedgerRefusesAFloodOfNewIssues(t *testing.T) {
	f := newFakeGitHub(t)
	out, err := runAgainst(t, f, "--max-new-issues", "2")
	if err == nil || !strings.Contains(err.Error(), "3 new flaky-test issues is more than --max-new-issues=2") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if len(f.writes) != 0 {
		t.Errorf("wrote %v despite the limit", f.writes)
	}
}

func TestDryRunClientRefusesWrites(t *testing.T) {
	f := newFakeGitHub(t)
	gh := &gitHub{api: f.srv.URL, token: "test-token", http: f.srv.Client(), readOnly: true}
	err := gh.post(context.Background(), "repos/"+fakeRepo+"/issues", map[string]string{"title": "x"}, nil)
	if err == nil || len(f.writes) != 0 {
		t.Errorf("read-only client sent a write: err=%v writes=%v", err, f.writes)
	}
}

func TestCommentBodyKeepsPullRequestTextInert(t *testing.T) {
	o := occurrence{
		failure: failure{Name: "TestX", Package: "internal/x", Excerpt: []string{"got ```@maintainer``` here"}},
		Kind:    kindRerun, PassedOn: 2, Source: "job log",
		Run: run{ID: 7, Name: "Tests", Event: "pull_request", HeadBranch: "feat/`@someone`", HeadSHA: "0123456789abcdef"},
		Job: job{ID: 99, RunAttempt: 1, StartedAt: time.Date(2026, 11, 3, 1, 2, 0, 0, time.UTC)},
	}
	o.Run.HeadRepository.FullName = "someone/LLMKube"
	body := commentBody(o, fakeRepo)
	if !strings.HasPrefix(body, jobMarker(99)+"\n") {
		t.Errorf("body does not start with its marker:\n%s", body)
	}
	if !strings.Contains(body, "\n````text\ngot ```@maintainer``` here\n````\n") {
		t.Errorf("excerpt fence does not outlast the backticks inside it:\n%s", body)
	}
	if !strings.Contains(body, "- Branch: `someone/LLMKube:feat/'@someone'` (pull_request)") {
		t.Errorf("branch is not rendered as one code span with its fork:\n%s", body)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
		"2026-10-20":           time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		"2026-10-20T06:00:00Z": time.Date(2026, 10, 20, 6, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0d", "-3d", "yesterday"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) succeeded", bad)
		}
	}
}

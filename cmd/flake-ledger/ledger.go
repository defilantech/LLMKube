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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const titlePrefix = "[FLAKY] "

type issue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	ClosedAt    *time.Time `json:"closed_at"`
	HTMLURL     string     `json:"html_url"`
	PullRequest *struct{}  `json:"pull_request"`
}

type comment struct {
	Body string `json:"body"`
}

// recordedMarker opens every occurrence comment. It must start the body, so a
// marker that a test printed into an excerpt cannot claim an occurrence.
var recordedMarker = regexp.MustCompile(`\A<!-- flake-ledger job=(\d+) -->`)

func jobMarker(id int64) string { return fmt.Sprintf("<!-- flake-ledger job=%d -->", id) }

// keyMarker identifies a flaky test's issue even after someone edits its title.
func keyMarker(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("<!-- flake-ledger key=%x -->", sum[:8])
}

func issueTitle(key string) string {
	t := []rune(titlePrefix + key)
	if len(t) > 250 {
		return string(t[:247]) + "..."
	}
	return string(t)
}

// entry is one flaky test and what the ledger does about it.
type entry struct {
	key      string
	occs     []occurrence
	open     *issue
	closed   *issue // the most recently closed earlier issue
	recorded int
	fresh    []occurrence
}

type ledger struct {
	gh      *gitHub
	repo    string
	label   string
	maxNew  int
	dryRun  bool
	verbose bool
	out     io.Writer
}

// record keeps one open issue per flaky test and comments on it once per
// occurrence. An occurrence already recorded on any of the test's issues,
// open or closed, is never posted again; one that happened before the test's
// last issue was closed does not open a new one, because it predates the fix.
func (l *ledger) record(ctx context.Context, occs []occurrence) error {
	entries := group(occs)
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(l.out, "no failures on main, in the merge queue, or passing on a rerun")
		return nil
	}
	issues, err := l.issues(ctx)
	if err != nil {
		return err
	}
	newIssues := 0
	for _, e := range entries {
		if err := l.plan(ctx, e, issues); err != nil {
			return err
		}
		if len(e.fresh) > 0 && e.open == nil {
			newIssues++
		}
	}
	verb := ""
	if l.dryRun {
		verb = "would "
	}
	for _, e := range entries {
		l.report(e, verb)
	}
	if newIssues > l.maxNew {
		msg := fmt.Sprintf("%d new flaky-test issues is more than --max-new-issues=%d; "+
			"that looks like a parsing fault or a broken main, so nothing was filed", newIssues, l.maxNew)
		if !l.dryRun {
			return fmt.Errorf("%s", msg)
		}
		_, _ = fmt.Fprintln(l.out, "warning: "+msg)
	}
	if l.dryRun {
		return nil
	}
	if newIssues > 0 {
		if err := l.ensureLabel(ctx); err != nil {
			return err
		}
	}
	for _, e := range entries {
		if err := l.apply(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// group collects the occurrences of each flaky test, oldest first.
func group(occs []occurrence) []*entry {
	byKey := map[string]*entry{}
	for _, o := range occs {
		e := byKey[o.key()]
		if e == nil {
			e = &entry{key: o.key()}
			byKey[o.key()] = e
		}
		dup := false
		for _, have := range e.occs {
			if have.Job.ID == o.Job.ID {
				dup = true
				break
			}
		}
		if !dup {
			e.occs = append(e.occs, o)
		}
	}
	out := make([]*entry, 0, len(byKey))
	for _, e := range byKey {
		sort.SliceStable(e.occs, func(a, b int) bool { return e.occs[a].Job.StartedAt.Before(e.occs[b].Job.StartedAt) })
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].key < out[b].key })
	return out
}

// issues lists every issue, open or closed, carrying the ledger's label.
func (l *ledger) issues(ctx context.Context) ([]issue, error) {
	q := url.Values{"labels": {l.label}, "state": {"all"}, "per_page": {"100"}}
	var out []issue
	err := l.gh.pages(ctx, fmt.Sprintf("repos/%s/issues?%s", l.repo, q.Encode()), func(b []byte) error {
		var page []issue
		if err := json.Unmarshal(b, &page); err != nil {
			return err
		}
		for _, i := range page {
			if i.PullRequest == nil {
				out = append(out, i)
			}
		}
		return nil
	})
	return out, err
}

// plan finds the test's issues and the occurrences none of them records yet.
func (l *ledger) plan(ctx context.Context, e *entry, issues []issue) error {
	title, marker := issueTitle(e.key), keyMarker(e.key)
	recorded := map[int64]bool{}
	for i := range issues {
		is := &issues[i]
		if is.Title != title && !strings.Contains(is.Body, marker) {
			continue
		}
		if is.State == "open" {
			if e.open == nil || is.Number > e.open.Number {
				e.open = is
			}
		} else if is.ClosedAt != nil && (e.closed == nil || is.ClosedAt.After(*e.closed.ClosedAt)) {
			e.closed = is
		}
		err := l.gh.pages(ctx, fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", l.repo, is.Number),
			func(b []byte) error {
				var page []comment
				if err := json.Unmarshal(b, &page); err != nil {
					return err
				}
				for _, c := range page {
					if m := recordedMarker.FindStringSubmatch(c.Body); m != nil {
						id, _ := strconv.ParseInt(m[1], 10, 64)
						recorded[id] = true
					}
				}
				return nil
			})
		if err != nil {
			return err
		}
	}
	for _, o := range e.occs {
		switch {
		case recorded[o.Job.ID]:
			e.recorded++
		case e.open == nil && e.closed != nil && !o.Job.StartedAt.After(*e.closed.ClosedAt):
			// Predates the fix that closed the last issue.
		default:
			e.fresh = append(e.fresh, o)
		}
	}
	return nil
}

func (l *ledger) report(e *entry, verb string) {
	title := issueTitle(e.key)
	switch {
	case len(e.fresh) == 0 && e.open != nil:
		_, _ = fmt.Fprintf(l.out, "%s: up to date on #%d (%d recorded)\n", title, e.open.Number, e.recorded)
	case len(e.fresh) == 0 && e.closed != nil:
		_, _ = fmt.Fprintf(l.out, "%s: nothing new since #%d was closed\n", title, e.closed.Number)
	case len(e.fresh) == 0:
		_, _ = fmt.Fprintf(l.out, "%s: already recorded (%d)\n", title, e.recorded)
	case e.open != nil:
		_, _ = fmt.Fprintf(l.out, "%s: %scomment on #%d with %d new occurrence(s) (%d already recorded)\n",
			title, verb, e.open.Number, len(e.fresh), e.recorded)
	default:
		_, _ = fmt.Fprintf(l.out, "%s: %sopen a new issue with %d occurrence(s)\n", title, verb, len(e.fresh))
	}
	for _, o := range e.fresh {
		_, _ = fmt.Fprintf(l.out, "    %s  %-13s  %s run %d attempt %d  %s@%s\n",
			o.Job.StartedAt.UTC().Format("2006-01-02 15:04"), o.Kind, o.Run.Name, o.Run.ID, o.Job.RunAttempt,
			branch(o.Run, l.repo), short(o.Run.HeadSHA))
		if l.verbose {
			for _, line := range strings.Split(commentBody(o, l.repo), "\n") {
				_, _ = fmt.Fprintln(l.out, "      | "+line)
			}
		}
	}
}

func (l *ledger) apply(ctx context.Context, e *entry) error {
	if len(e.fresh) == 0 {
		return nil
	}
	if e.open == nil {
		var created issue
		in := map[string]any{
			"title":  issueTitle(e.key),
			"body":   issueBody(e),
			"labels": []string{l.label},
		}
		if err := l.gh.post(ctx, fmt.Sprintf("repos/%s/issues", l.repo), in, &created); err != nil {
			return err
		}
		e.open = &created
		_, _ = fmt.Fprintf(l.out, "opened #%d %s\n", created.Number, created.HTMLURL)
	}
	for _, o := range e.fresh {
		p := fmt.Sprintf("repos/%s/issues/%d/comments", l.repo, e.open.Number)
		if err := l.gh.post(ctx, p, map[string]string{"body": commentBody(o, l.repo)}, nil); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(l.out, "recorded %d occurrence(s) on #%d\n", len(e.fresh), e.open.Number)
	return nil
}

// ensureLabel creates the label the first time the ledger opens an issue.
func (l *ledger) ensureLabel(ctx context.Context) error {
	_, _, err := l.gh.get(ctx, fmt.Sprintf("repos/%s/labels/%s", l.repo, url.PathEscape(l.label)))
	if err == nil {
		return nil
	}
	if !isGone(err) {
		return err
	}
	return l.gh.post(ctx, fmt.Sprintf("repos/%s/labels", l.repo), map[string]string{
		"name":        l.label,
		"color":       "d93f0b",
		"description": "A test failed on main or passed on a rerun; one issue per test (flake ledger)",
	}, nil)
}

func issueBody(e *entry) string {
	f := e.occs[0].failure
	var b strings.Builder
	b.WriteString(keyMarker(e.key) + "\n")
	fmt.Fprintf(&b, "%s in %s failed on `main`, in the merge queue, or on a run attempt that a rerun of "+
		"the same commit then passed. A failure like that says nothing about the change under test, so it "+
		"is a flaky test. The flake ledger (`.github/workflows/flake-ledger.yml`) keeps one open issue per "+
		"flaky test and adds a comment here for each new occurrence.\n\n", code(f.Name), code(orDot(f.Package)))
	b.WriteString("Fix the cause, or skip the test with this issue's number in the skip message until it is " +
		"fixed: a test that flakes on `main` twice in a week is fixed or quarantined within a week " +
		"(CONTRIBUTING.md, \"A test flaked\"). Close this issue with the fix. If the test flakes again " +
		"after that, the ledger opens a new issue.\n")
	if e.closed != nil {
		fmt.Fprintf(&b, "\nPreviously tracked in #%d.\n", e.closed.Number)
	}
	return b.String()
}

func commentBody(o occurrence, repo string) string {
	var b strings.Builder
	b.WriteString(jobMarker(o.Job.ID) + "\n")
	switch o.Kind {
	case kindMain:
		b.WriteString("**main**: failed on a push to `main`.")
	case kindMergeQueue:
		b.WriteString("**merge-queue**: failed in the merge queue.")
	case kindRerun:
		fmt.Fprintf(&b, "**pass-on-rerun**: failed on attempt %d, then passed on attempt %d of the same "+
			"run (same commit).", o.Job.RunAttempt, o.PassedOn)
	}
	if o.Kind != kindRerun && o.PassedOn > 0 {
		fmt.Fprintf(&b, " Attempt %d of the same run passed.", o.PassedOn)
	}
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "- Run: [%s run %d, attempt %d](%s)\n", o.Run.Name, o.Run.ID, o.Job.RunAttempt, jobURL(o, repo))
	fmt.Fprintf(&b, "- Commit: [%s](https://github.com/%s/commit/%s)\n", code(short(o.Run.HeadSHA)), repo, o.Run.HeadSHA)
	fmt.Fprintf(&b, "- Branch: %s (%s)\n", code(branch(o.Run, repo)), o.Run.Event)
	fmt.Fprintf(&b, "- Started: %s\n", o.Job.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "- Read from: %s\n", o.Source)
	if len(o.Excerpt) > 0 {
		text := strings.Join(o.Excerpt, "\n")
		fence := strings.Repeat("`", max(3, longestRun(text, '`')+1))
		fmt.Fprintf(&b, "\n%stext\n%s\n%s\n", fence, text, fence)
	}
	return b.String()
}

func jobURL(o occurrence, repo string) string {
	if o.Job.HTMLURL != "" {
		return o.Job.HTMLURL
	}
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", repo, o.Run.ID, o.Job.ID)
}

// branch names a fork's branch with its repository, since fork runs have no
// pull_requests entry to say whose branch it is.
func branch(r run, repo string) string {
	if fork := r.HeadRepository.FullName; fork != "" && !strings.EqualFold(fork, repo) {
		return fork + ":" + r.HeadBranch
	}
	return r.HeadBranch
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func orDot(pkg string) string {
	if pkg == "" {
		return "."
	}
	return pkg
}

// code renders inline code. Branch names and Ginkgo spec text come from pull
// requests, so a backtick in them must not end the span and let the rest
// render as markdown.
func code(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "'") + "`"
}

func longestRun(s string, c byte) int {
	longest, n := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n++
			longest = max(longest, n)
		} else {
			n = 0
		}
	}
	return longest
}

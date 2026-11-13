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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// failure is one failing test in one job attempt: a top-level Go test, a
// Ginkgo spec, or a package whose test binary died outside any test (a
// -timeout kill).
type failure struct {
	Name    string
	Package string // module-relative directory, such as internal/controller
	Excerpt []string
}

// key names the flaky test across runs. The package is part of it because
// top-level test names repeat across packages (TestParseRepo, TestSanitizeName).
func (f failure) key() string {
	if f.Package == "" {
		return f.Name
	}
	return f.Name + " (" + f.Package + ")"
}

const (
	maxExcerptLines = 15
	maxLineLen      = 240
	timeoutName     = "go test timeout"
	exitName        = "test binary exited outside a test"
)

var (
	logTimestamp = regexp.MustCompile(`^\x{FEFF}?\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
	// test2json drops the ESC byte of Ginkgo's colors and keeps the rest, so
	// the Tests job's report and its log carry bare "[38;5;9m" codes too.
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]|\[[0-9]+(;[0-9]+)*m`)

	goFailLine     = regexp.MustCompile(`^\s*--- FAIL: (\S+) \(`)
	goPkgFailLine  = regexp.MustCompile(`^FAIL\t(\S+)`)
	goFileLine     = regexp.MustCompile(`^\s+[\w./-]+\.go:\d+: `)
	goTimeoutPanic = regexp.MustCompile(`panic: test timed out after`)

	ginkgoSummary = regexp.MustCompile(`^Summarizing \d+ Failures?:`)
	ginkgoRan     = regexp.MustCompile(`^Ran \d+ of \d+ Specs`)
	ginkgoEnd     = regexp.MustCompile(`^(FAIL|SUCCESS)! -- `)
	// Summary entries. [INTERRUPTED] and [ABORTED] are left out: they are
	// what a cancelled job prints, not a failing spec.
	ginkgoEntry   = regexp.MustCompile(`^\s*\[(FAIL|PANICKED!|TIMEDOUT)\]\s+(.*?)\s*$`)
	ginkgoMessage = regexp.MustCompile(`^\s*\[(FAILED|PANICKED|TIMEDOUT)\] (.*)$`)
	ginkgoEarly   = regexp.MustCompile(`^\s*\[(FAILED|PANICKED|TIMEDOUT)\] in \[`)
	ginkgoAt      = regexp.MustCompile(`^\s*In \[[^\]]+\] at: (\S+)`)
	ginkgoSuite   = regexp.MustCompile(`^Running Suite: .* - (/\S+)$`)
	// A spec location inside the checked-out repository names its package.
	workspaceFile = regexp.MustCompile(`/home/runner/work/[^/]+/[^/]+/(.+)/[^/]+\.go:\d+`)
	workspaceDir  = regexp.MustCompile(`^/home/runner/work/[^/]+/[^/]+/(.+)$`)
)

// cleanLog splits a job log into lines without the runner's timestamps or
// ANSI colors.
func cleanLog(raw []byte) []string {
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		l = logTimestamp.ReplaceAllString(l, "")
		lines[i] = ansiEscape.ReplaceAllString(l, "")
	}
	return lines
}

// relPackage turns an import path into the module-relative directory.
func relPackage(module, pkg string) string {
	if pkg == module {
		return "."
	}
	return strings.TrimPrefix(pkg, module+"/")
}

// clip bounds an excerpt so a comment stays short whatever the test printed.
func clip(lines []string, keepLast bool) []string {
	if len(lines) > maxExcerptLines {
		if keepLast {
			lines = lines[len(lines)-maxExcerptLines:]
		} else {
			lines = lines[:maxExcerptLines]
		}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if len(l) > maxLineLen {
			l = l[:maxLineLen] + " ..."
		}
		out = append(out, l)
	}
	return out
}

// testEvent is one line of `go test -json` (cmd/test2json).
type testEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	OutputType  string
	FailedBuild string
}

type outLine struct {
	text  string
	error bool // OutputType "error": t.Error/t.Fatal text (Go 1.25+)
}

type testState struct {
	pkg, name string
	out       []outLine
	failed    bool
	done      bool
}

type pkgState struct {
	failed        bool
	build         bool // failed to build: not a test failure
	timedOut      bool
	hasFailedTest bool
	out           []string // package-level output, and everything after a timeout panic
}

// goReport accumulates one go test -json stream, keyed by top-level test:
// a subtest's output and result belong to the test that ran it.
type goReport struct {
	tests map[string]*testState
	order []*testState
	pkgs  map[string]*pkgState
}

func (g *goReport) test(pkg, test string) *testState {
	name, _, _ := strings.Cut(test, "/")
	k := pkg + "\x00" + name
	s := g.tests[k]
	if s == nil {
		s = &testState{pkg: pkg, name: name}
		g.tests[k] = s
		g.order = append(g.order, s)
	}
	return s
}

func (g *goReport) pkg(name string) *pkgState {
	p := g.pkgs[name]
	if p == nil {
		p = &pkgState{}
		g.pkgs[name] = p
	}
	return p
}

func (g *goReport) add(e testEvent) {
	p := g.pkg(e.Package)
	topLevel := e.Test != "" && !strings.Contains(e.Test, "/")
	switch e.Action {
	case "output":
		text := strings.TrimRight(e.Output, "\n")
		if goTimeoutPanic.MatchString(text) {
			p.timedOut = true
		}
		if e.Test != "" {
			s := g.test(e.Package, e.Test)
			s.out = append(s.out, outLine{text: text, error: e.OutputType == "error"})
		}
		if e.Test == "" || p.timedOut {
			p.out = append(p.out, text)
		}
	case "run":
		g.test(e.Package, e.Test)
	case "pass", "skip":
		if topLevel {
			g.test(e.Package, e.Test).done = true
		}
	case "fail":
		switch {
		case e.Test == "":
			p.failed = true
			p.build = e.FailedBuild != ""
		case topLevel:
			s := g.test(e.Package, e.Test)
			s.failed, s.done = true, true
			p.hasFailedTest = true
		}
	}
}

// failures returns the failed top-level tests, the specs of any Ginkgo suite
// among them, and each package that failed outside any test.
func (g *goReport) failures(module string) []failure {
	var out []failure
	for _, s := range g.order {
		if !s.failed {
			continue
		}
		rel := relPackage(module, s.pkg)
		text := make([]string, len(s.out))
		for i, o := range s.out {
			text[i] = ansiEscape.ReplaceAllString(o.text, "")
		}
		// A Ginkgo suite is one go test whose failures are its specs.
		if specs := parseGinkgo(text, rel); len(specs) > 0 {
			out = append(out, specs...)
			continue
		}
		out = append(out, failure{Name: s.name, Package: rel, Excerpt: goExcerpt(s.out)})
	}
	for _, name := range sortedKeys(g.pkgs) {
		p := g.pkgs[name]
		if !p.failed || p.build || p.hasFailedTest {
			continue
		}
		var running []string
		for _, s := range g.order {
			if s.pkg == name && !s.done {
				running = append(running, s.name)
			}
		}
		out = append(out, packageFailure(relPackage(module, name), p.timedOut, p.out, running))
	}
	return out
}

// parseGoTestJSON reads a go-test-report file: `go test -json` events
// interleaved with make's plain lines. It returns the failed top-level tests,
// the failed specs of any Ginkgo suite among them, and every package that
// failed with no failing test and no build failure (a -timeout kill).
func parseGoTestJSON(r io.Reader, module string) ([]failure, error) {
	g := &goReport{tests: map[string]*testState{}, pkgs: map[string]*pkgState{}}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
			var e testEvent
			if json.Unmarshal(line, &e) == nil && e.Action != "" && e.Package != "" {
				g.add(e)
			}
		}
		if errors.Is(err, io.EOF) {
			return g.failures(module), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// packageFailure records a test binary that died with no failing test. A
// -timeout kill reports no result for the test it interrupted, so the panic
// and the tests still running are the evidence.
func packageFailure(pkg string, timedOut bool, out, running []string) failure {
	f := failure{Name: exitName, Package: pkg}
	if timedOut {
		f.Name = timeoutName
		for i, l := range out {
			if goTimeoutPanic.MatchString(l) {
				end := i + 1
				for end < len(out) && (strings.HasPrefix(out[end], "\t") || strings.TrimSpace(out[end]) == "running tests:") {
					end++
				}
				f.Excerpt = clip(out[i:end], false)
				return f
			}
		}
	}
	if len(running) > 0 {
		f.Excerpt = append(f.Excerpt, "still running when the package exited:")
		for _, t := range running {
			f.Excerpt = append(f.Excerpt, "\t"+t)
		}
		f.Excerpt = clip(f.Excerpt, false)
		return f
	}
	f.Excerpt = clip(out, true)
	return f
}

// goExcerpt picks the lines that say why a test failed: the t.Error and
// t.Fatal text when the toolchain marks it, otherwise the file:line messages
// and FAIL frames, plus the head of any panic.
func goExcerpt(out []outLine) []string {
	var marked, guessed []string
	for i, o := range out {
		switch {
		case o.error:
			marked = append(marked, o.text)
		case goFailLine.MatchString(o.text):
			marked = append(marked, o.text)
			guessed = append(guessed, o.text)
		case goFileLine.MatchString(o.text):
			guessed = append(guessed, o.text)
		case strings.HasPrefix(o.text, "panic: "):
			end := min(i+6, len(out))
			for _, p := range out[i:end] {
				marked = append(marked, p.text)
				guessed = append(guessed, p.text)
			}
		}
	}
	hasMarked := false
	for _, o := range out {
		if o.error {
			hasMarked = true
			break
		}
	}
	if hasMarked {
		return clip(marked, false)
	}
	if len(guessed) > 0 {
		return clip(guessed, true)
	}
	var rest []string
	for _, o := range out {
		if !strings.HasPrefix(o.text, "=== ") {
			rest = append(rest, o.text)
		}
	}
	return clip(rest, true)
}

// parseGinkgo reads the "Summarizing N Failures" blocks Ginkgo prints at the
// end of a suite and returns one failure per failed spec, with the spec's
// [FAILED] message as the excerpt. A spec's package comes from its failure
// location; when that is outside the repository (a BeforeSuite failing inside
// Ginkgo's own code) it comes from the suite's "Running Suite" header, and
// failing that from defaultPkg.
func parseGinkgo(lines []string, defaultPkg string) []failure {
	type message struct {
		at   string
		text []string
	}
	var messages []message
	var cur *message
	var out []failure
	seen := map[string]bool{}
	inSummary := false
	suitePkg := defaultPkg
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if m := ginkgoSuite.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			if dm := workspaceDir.FindStringSubmatch(m[1]); dm != nil {
				suitePkg = dm[1]
			}
			continue
		}
		switch {
		case ginkgoSummary.MatchString(strings.TrimSpace(l)):
			inSummary = true
			continue
		case ginkgoRan.MatchString(strings.TrimSpace(l)):
			inSummary = false
			continue
		}
		if inSummary {
			m := ginkgoEntry.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			f := failure{Name: m[2], Package: suitePkg}
			loc := ""
			if i+1 < len(lines) {
				loc = strings.TrimSpace(lines[i+1])
				if pm := workspaceFile.FindStringSubmatch(loc); pm != nil {
					f.Package = pm[1]
				}
			}
			for _, msg := range messages {
				if loc != "" && msg.at == loc {
					f.Excerpt = clip(msg.text, false)
					break
				}
			}
			if f.Excerpt == nil {
				f.Excerpt = []string{strings.TrimSpace(l), loc}
			}
			if !seen[f.key()] {
				seen[f.key()] = true
				out = append(out, f)
			}
			continue
		}
		if cur != nil {
			if m := ginkgoAt.FindStringSubmatch(l); m != nil {
				cur.at = m[1]
				messages = append(messages, *cur)
				cur = nil
			} else if len(cur.text) < 200 {
				cur.text = append(cur.text, strings.TrimSpace(l))
			}
			continue
		}
		if ginkgoMessage.MatchString(l) && !ginkgoEarly.MatchString(l) {
			cur = &message{text: []string{strings.TrimSpace(l)}}
		}
	}
	return out
}

// parseGoTestLog is the fallback when a Tests job's go-test-report artifact
// has expired or was never uploaded. It reads the timing report the job
// prints from that same file (hack/go-test-report.sh summary), or, in logs
// from before the report existed, the `--- FAIL` lines and the `FAIL <pkg>`
// line that follows them.
func parseGoTestLog(lines []string, module string) []failure {
	var out []failure
	add := func(f failure) {
		for _, o := range out {
			if o.key() == f.key() {
				return
			}
		}
		out = append(out, f)
	}
	timedOut := false
	for _, l := range lines {
		if goTimeoutPanic.MatchString(l) {
			timedOut = true
			break
		}
	}

	failed, running, ok := timingReport(lines)
	if ok {
		hasFailed := map[string]bool{}
		for _, ft := range failed {
			hasFailed[ft[0]] = true
			if !ginkgoRunner(lines, ft[1]) {
				add(failure{Name: ft[1], Package: ft[0], Excerpt: logExcerpt(lines, ft[1])})
			}
		}
		for _, pkg := range sortedKeys(running) {
			if !hasFailed[pkg] {
				add(packageFailure(pkg, timedOut, timeoutLines(lines), running[pkg]))
			}
		}
	} else {
		var pending []string
		pendingTimeout := false
		for _, l := range lines {
			if m := goFailLine.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, " ") &&
				!strings.Contains(m[1], "/") && !ginkgoRunner(lines, m[1]) {
				pending = append(pending, m[1])
				continue
			}
			if goTimeoutPanic.MatchString(l) {
				pendingTimeout = true
				continue
			}
			if m := goPkgFailLine.FindStringSubmatch(l); m != nil {
				pkg := relPackage(module, m[1])
				if strings.Contains(l, "[build failed]") || strings.Contains(l, "[setup failed]") {
					pending, pendingTimeout = nil, false
					continue
				}
				for _, t := range pending {
					add(failure{Name: t, Package: pkg, Excerpt: logExcerpt(lines, t)})
				}
				if len(pending) == 0 && pendingTimeout {
					add(packageFailure(pkg, true, timeoutLines(lines), nil))
				}
				pending, pendingTimeout = nil, false
			}
		}
	}
	return append(out, parseGinkgo(lines, "")...)
}

// timingReport reads the "== Failed tests" and "== Still running" sections
// of hack/go-test-report.sh summary. failed holds [package, test] pairs of
// top-level tests; running maps a package to the tests a kill interrupted.
func timingReport(lines []string) (failed [][2]string, running map[string][]string, ok bool) {
	running = map[string][]string{}
	section := ""
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "== Failed tests"):
			section, ok = "failed", true
			continue
		case strings.HasPrefix(l, "== Still running"):
			section = "running"
			continue
		case strings.HasPrefix(l, "== "), strings.TrimSpace(l) == "":
			section = ""
			continue
		}
		f := strings.Split(l, "\t")
		switch {
		case section == "failed" && len(f) == 3 && !strings.Contains(f[2], "/"):
			failed = append(failed, [2]string{f[1], f[2]})
		case section == "running" && len(f) == 2 && !strings.Contains(f[1], "/"):
			running[f[0]] = append(running[f[0]], f[1])
		}
	}
	return failed, running, ok
}

// ginkgoRunner reports whether test is the go test that ran a Ginkgo suite:
// its FAIL line comes right after Ginkgo's "Ran N of M Specs" footer. Its
// failures are the specs, which parseGinkgo reports.
func ginkgoRunner(lines []string, test string) bool {
	for i, l := range lines {
		m := goFailLine.FindStringSubmatch(l)
		if m == nil || m[1] != test {
			continue
		}
		for j := max(0, i-5); j < i; j++ {
			t := strings.TrimSpace(lines[j])
			if ginkgoRan.MatchString(t) || ginkgoEnd.MatchString(t) {
				return true
			}
		}
	}
	return false
}

// logExcerpt takes the file:line messages around each FAIL line of test and
// its subtests: above it in a -v or -json stream, which prints a test's output
// as it runs, and indented below it in plain go test output, which prints it
// after the result.
func logExcerpt(lines []string, test string) []string {
	var out []string
	for i, l := range lines {
		m := goFailLine.FindStringSubmatch(l)
		if m == nil || (m[1] != test && !strings.HasPrefix(m[1], test+"/")) {
			continue
		}
		start := i
		for start > 0 && i-start < 8 && goFileLine.MatchString(lines[start-1]) {
			start--
		}
		end := i + 1
		for end < len(lines) && end-i <= 8 && goFileLine.MatchString(lines[end]) {
			end++
		}
		out = append(out, lines[start:end]...)
	}
	return clip(out, false)
}

// timeoutLines returns the timeout panic and the list of running tests under it.
func timeoutLines(lines []string) []string {
	for i, l := range lines {
		if goTimeoutPanic.MatchString(l) {
			end := min(i+1+maxExcerptLines, len(lines))
			return lines[i:end]
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

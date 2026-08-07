package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseUnifiedDiff_AttributesAddedAndRemoved(t *testing.T) {
	// A modified test file: one assertion removed, one added.
	out := `diff --git a/pkg/model/x_test.go b/pkg/model/x_test.go
index 1111111..2222222 100644
--- a/pkg/model/x_test.go
+++ b/pkg/model/x_test.go
@@ -10 +10 @@ func TestFoo(t *testing.T) {
-	Expect(got).To(Equal(oldWant))
+	Expect(got).To(Equal(newWant))
`
	got := parseUnifiedDiff(out)
	fh := got["pkg/model/x_test.go"]
	if fh == nil {
		t.Fatalf("no hunks for pkg/model/x_test.go; got keys %v", keys(got))
	}
	if !reflect.DeepEqual(fh.Removed, []string{"\tExpect(got).To(Equal(oldWant))"}) {
		t.Errorf("Removed = %q", fh.Removed)
	}
	if !reflect.DeepEqual(fh.Added, []string{"\tExpect(got).To(Equal(newWant))"}) {
		t.Errorf("Added = %q", fh.Added)
	}
}

func TestParseUnifiedDiff_DeletedFileAttributedToOldPath(t *testing.T) {
	out := `diff --git a/pkg/model/y_test.go b/pkg/model/y_test.go
deleted file mode 100644
index 3333333..0000000
--- a/pkg/model/y_test.go
+++ /dev/null
@@ -1 +0,0 @@
-	require.NoError(t, err)
`
	got := parseUnifiedDiff(out)
	fh := got["pkg/model/y_test.go"]
	if fh == nil || len(fh.Removed) != 1 {
		t.Fatalf("deleted file removed lines not attributed to old path; got %v", keys(got))
	}
}

// keys is a tiny test helper for readable failure messages.
func keys(m map[string]*fileHunks) []string {
	k := make([]string, 0, len(m))
	for f := range m {
		k = append(k, f)
	}
	return k
}

func TestAssertionErosion_NetRemovalCountedWithSnippets(t *testing.T) {
	fh := &fileHunks{
		Removed: []string{
			"\tExpect(got).To(Equal(want))",
			"\trequire.NoError(t, err)",
			"\t// just a comment, not an assertion",
		},
		Added: []string{
			"\tassert.Equal(t, want, got)",
		},
	}
	removed, added, snippets := assertionErosion(fh)
	if removed != 2 || added != 1 {
		t.Fatalf("removed=%d added=%d, want 2 and 1", removed, added)
	}
	if len(snippets) != 2 || snippets[0] != "Expect(got).To(Equal(want))" {
		t.Errorf("snippets = %q", snippets)
	}
}

func TestAssertionErosion_NonAssertionsIgnored(t *testing.T) {
	fh := &fileHunks{Removed: []string{"\tx := 1", "\treturn nil"}}
	removed, _, _ := assertionErosion(fh)
	if removed != 0 {
		t.Fatalf("removed=%d, want 0 (no assertion-shaped lines)", removed)
	}
}

func TestFirstN(t *testing.T) {
	if got := firstN([]string{"a", "b", "c"}, 2); len(got) != 2 {
		t.Fatalf("firstN cap failed: %v", got)
	}
	if got := firstN([]string{"a"}, 3); len(got) != 1 {
		t.Fatalf("firstN under-length failed: %v", got)
	}
}

func TestFixtureLiteralChurn_HostRelocation(t *testing.T) {
	// The #1322 shape: a fixture URL host moved off huggingface.co.
	fh := &fileHunks{
		Removed: []string{`	src := "https://huggingface.co/org/model/resolve/main/f.gguf"`},
		Added:   []string{`	src := "https://example.com/org/model/resolve/main/f.gguf"`},
	}
	got := fixtureLiteralChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "huggingface.co") || !strings.Contains(got[0], "example.com") {
		t.Fatalf("expected a host-churn finding naming both hosts; got %q", got)
	}
}

func TestFixtureLiteralChurn_PureAdditionSilent(t *testing.T) {
	// Adding a new fixture (no matching removal) is not relocation.
	fh := &fileHunks{
		Added: []string{`	src := "https://huggingface.co/org/model/f.gguf"`},
	}
	if got := fixtureLiteralChurn(fh); got != nil {
		t.Fatalf("pure addition must not flag churn; got %q", got)
	}
}

func TestFixtureLiteralChurn_TestdataPathRelocation(t *testing.T) {
	fh := &fileHunks{
		Removed: []string{`	data := load("pkg/model/testdata/real_repo.json")`},
		Added:   []string{`	data := load("pkg/model/testdata/renamed_repo.json")`},
	}
	got := fixtureLiteralChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "testdata/") {
		t.Fatalf("expected a testdata path-churn finding; got %q", got)
	}
}

func TestAssertionValueChurn_EqualValueRewritten(t *testing.T) {
	// #1347 shape: the coder changed Equal(42) to Equal(0) to match buggy behavior.
	fh := &fileHunks{
		Removed: []string{`	Expect(got).To(Equal(42))`},
		Added:   []string{`	Expect(got).To(Equal(0))`},
	}
	got := assertionValueChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "Equal(42)") || !strings.Contains(got[0], "Equal(0)") {
		t.Fatalf("expected an assertion-value-churn finding naming both values; got %q", got)
	}
}

func TestAssertionValueChurn_ContainSubstringValueRewritten(t *testing.T) {
	fh := &fileHunks{
		Removed: []string{`	Expect(got).To(ContainSubstring("boom"))`},
		Added:   []string{`	Expect(got).To(ContainSubstring("ok"))`},
	}
	got := assertionValueChurn(fh)
	if len(got) != 1 || !strings.Contains(got[0], "ContainSubstring") {
		t.Fatalf("expected a ContainSubstring churn finding; got %q", got)
	}
}

func TestAssertionValueChurn_PureAdditionSilent(t *testing.T) {
	// Adding a new assertion (no matching removal) is not churn.
	fh := &fileHunks{
		Added: []string{`	Expect(got).To(Equal(42))`},
	}
	if got := assertionValueChurn(fh); got != nil {
		t.Fatalf("pure addition must not flag churn; got %q", got)
	}
}

func TestAssertionValueChurn_PureDeletionSilent(t *testing.T) {
	// Removing an assertion (no matching addition) is not churn (caught by erosion).
	fh := &fileHunks{
		Removed: []string{`	Expect(got).To(Equal(42))`},
	}
	if got := assertionValueChurn(fh); got != nil {
		t.Fatalf("pure deletion must not flag churn; got %q", got)
	}
}

func TestAssertionValueChurn_SameValueSilent(t *testing.T) {
	// The assertion value did not change: no churn.
	fh := &fileHunks{
		Removed: []string{`	Expect(got).To(Equal(42))`},
		Added:   []string{`	Expect(got).To(Equal(42))`},
	}
	if got := assertionValueChurn(fh); got != nil {
		t.Fatalf("same value must not flag churn; got %q", got)
	}
}

func TestParseNameStatus_ModifyAndRename(t *testing.T) {
	out := "M\tpkg/model/classifier.go\n" +
		"D\tpkg/model/testdata/real.json\n" +
		"R100\tpkg/model/testdata/a.json\tpkg/model/testdata/b.json\n"
	got := parseNameStatus(out)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}
	wantOldPath := "pkg/model/testdata/a.json"
	wantNewPath := "pkg/model/testdata/b.json"
	if got[2].Code[0] != 'R' || got[2].OldPath != wantOldPath || got[2].Path != wantNewPath {
		t.Errorf("rename parsed wrong: %+v", got[2])
	}
}

func TestChangedProdPackages_IgnoresTestAndNonGo(t *testing.T) {
	entries := []nameStatusEntry{
		{Code: "M", Path: "pkg/model/classifier.go"},
		{Code: "M", Path: "pkg/model/classifier_test.go"},
		{Code: "M", Path: "pkg/other/x_test.go"}, // test-only pkg: not prod-changed
		{Code: "M", Path: "docs/readme.md"},
	}
	got := changedProdPackages(entries)
	if !got["pkg/model"] {
		t.Errorf("pkg/model should be a changed-prod package")
	}
	if got["pkg/other"] {
		t.Errorf("pkg/other changed only a test file; must not count as prod-changed")
	}
	if len(got) != 1 {
		t.Errorf("got %v, want only pkg/model", got)
	}
}

func TestTestdataOwner(t *testing.T) {
	if o, ok := testdataOwner("pkg/model/testdata/x.json"); !ok || o != "pkg/model" {
		t.Errorf("owner = %q, %v; want pkg/model, true", o, ok)
	}
	if _, ok := testdataOwner("pkg/model/classifier.go"); ok {
		t.Errorf("non-testdata path must not resolve an owner")
	}
}

func TestFixtureFileChanges_DeleteAndRenameUnderChangedPkg(t *testing.T) {
	entries := []nameStatusEntry{
		{Code: "D", Path: "pkg/model/testdata/real.json"},
		{Code: "R100", OldPath: "pkg/model/testdata/a.json", Path: "pkg/model/testdata/b.json"},
		{Code: "D", Path: "pkg/other/testdata/z.json"}, // owner not prod-changed: ignored
	}
	prod := map[string]bool{"pkg/model": true}
	got := fixtureFileChanges(entries, prod)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if !strings.Contains(joined, "deleted fixture pkg/model/testdata/real.json") ||
		!strings.Contains(joined, "relocated fixture pkg/model/testdata/a.json -> pkg/model/testdata/b.json") {
		t.Errorf("findings = %v", got)
	}
}

func TestFixtureFileChanges_CrossPackageRenameOutOfChangedPkgFires(t *testing.T) {
	// Moved OUT of a changed package into an untouched one: the changed
	// package lost the fixture, so this must fire (was a false negative).
	entries := []nameStatusEntry{
		{Code: "R100", OldPath: "pkg/model/testdata/a.json", Path: "pkg/other/testdata/a.json"},
	}
	prod := map[string]bool{"pkg/model": true}
	got := fixtureFileChanges(entries, prod)
	if len(got) != 1 || !strings.Contains(got[0], "pkg/model/testdata/a.json -> pkg/other/testdata/a.json") {
		t.Fatalf("expected a relocation finding attributed to the changed source package; got %v", got)
	}
}

func TestFixtureFileChanges_CrossPackageRenameIntoChangedPkgSilent(t *testing.T) {
	// Moved INTO a changed package from an untouched one: nothing was lost
	// from the changed package, so this must stay silent (was a false positive).
	entries := []nameStatusEntry{
		{Code: "R100", OldPath: "pkg/other/testdata/a.json", Path: "pkg/model/testdata/a.json"},
	}
	prod := map[string]bool{"pkg/model": true}
	if got := fixtureFileChanges(entries, prod); len(got) != 0 {
		t.Fatalf("a fixture moved INTO the changed package must not fire; got %v", got)
	}
}

// dilutionRunner fakes the three git calls checkTestDilution makes:
// `git add -A` (no-op), `git diff --name-status --cached HEAD`, and the
// `git diff --cached --unified=0 ... -- *_test.go` line diff.
func dilutionRunner(nameStatus, testDiff string, addErr, nsErr, diffErr error) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) >= 2 && args[0] == "add" && args[1] == "-A":
			return "", addErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--name-status":
			return nameStatus, nsErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--cached":
			return testDiff, diffErr
		default:
			return "", nil
		}
	}
}

// erosionNS + erosionDiff describe a genuine dilution: a changed-prod package
// (pkg/model) whose test net-removes an assertion. A fail-open test feeds these
// plus one non-nil git error, so that WITHOUT the checked error branch execution
// would reach this finding and return (true, ...); only the fail-open return
// keeps it silent. That makes each fail-open test isolate its branch.
const erosionNS = "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
const erosionDiff = `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +9 @@
-	Expect(classify(u)).To(Equal(RepoSource))
`

func TestCheckTestDilution_FiresOnNetRemovedAssertions(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +10 @@
-	Expect(classify(u)).To(Equal(RepoSource))
-	require.NoError(t, err)
+	// removed the assertions above
`
	failed, out := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil))
	if !failed {
		t.Fatal("expected an advisory when a changed-prod package net-removes assertions")
	}
	if !strings.Contains(out, "classifier_test.go") || !strings.Contains(out, "assertion") {
		t.Errorf("detail = %q", out)
	}
}

func TestCheckTestDilution_FiresOnFixtureRelocation_1322Shape(t *testing.T) {
	// #1322 bite-check: prod classifier changed, and a fixture URL host moved
	// off huggingface.co in the same package's test.
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +20 @@
-	src := "https://huggingface.co/org/model/resolve/main/f.gguf"
+	src := "https://example.com/org/model/resolve/main/f.gguf"
`
	failed, out := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil))
	if !failed {
		t.Fatal("expected an advisory for the #1322 fixture-relocation shape")
	}
	if !strings.Contains(out, "huggingface.co") {
		t.Errorf("detail should name the moved host; got %q", out)
	}
}

func TestCheckTestDilution_FiresOnAssertionValueChurn_1347Shape(t *testing.T) {
	// #1347 shape: prod classifier changed, and an assertion's expected value
	// was rewritten from Equal(42) to Equal(0) in the same package's test.
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +10 @@
-	Expect(classify(u)).To(Equal(42))
+	Expect(classify(u)).To(Equal(0))
`
	failed, out := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil))
	if !failed {
		t.Fatal("expected an advisory for the #1347 assertion-value-churn shape")
	}
	if !strings.Contains(out, "Equal(42)") || !strings.Contains(out, "Equal(0)") {
		t.Errorf("detail should name both assertion values; got %q", out)
	}
}

func TestCheckTestDilution_SilentWhenTestsOnlyGrow(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +11 @@
+	Expect(classify(u)).To(Equal(RepoSource))
`
	if failed, _ := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil)); failed {
		t.Fatal("adding assertions must not fire the dilution advisory")
	}
}

func TestCheckTestDilution_SilentWhenNoProdChange(t *testing.T) {
	// Test-only submission: assertions removed but no production code changed.
	ns := "M\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -10 +9 @@
-	Expect(classify(u)).To(Equal(RepoSource))
`
	if failed, _ := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil)); failed {
		t.Fatal("package-linkage: no production change means no advisory")
	}
}

func TestCheckTestDilution_FailOpenOnGitError(t *testing.T) {
	// name-status call errors: must fail open even though a real dilution is present.
	if failed, out := checkTestDilution(context.Background(), "/w",
		dilutionRunner(erosionNS, erosionDiff, nil, errors.New("boom"), nil)); failed || out != "" {
		t.Fatalf("name-status error must fail open (silent) despite a real dilution; got failed=%v out=%q", failed, out)
	}
}

func TestCheckTestDilution_FailOpenOnAddError(t *testing.T) {
	// git add -A errors: must fail open even though a real dilution is present.
	if failed, out := checkTestDilution(context.Background(), "/w",
		dilutionRunner(erosionNS, erosionDiff, errors.New("boom"), nil, nil)); failed || out != "" {
		t.Fatalf("git add error must fail open (silent) despite a real dilution; got failed=%v out=%q", failed, out)
	}
}

func TestCheckTestDilution_FailOpenOnDiffError(t *testing.T) {
	// unified-diff call errors: must fail open even though a real dilution is present.
	if failed, out := checkTestDilution(context.Background(), "/w",
		dilutionRunner(erosionNS, erosionDiff, nil, nil, errors.New("boom"))); failed || out != "" {
		t.Fatalf("unified-diff error must fail open (silent) despite a real dilution; got failed=%v out=%q", failed, out)
	}
}

func TestCheckTestDilution_SilentWhenErosionInUnchangedPackage(t *testing.T) {
	// Production changed in pkg/model, but the weakened test is in pkg/other,
	// which has no production change: per-file package linkage must keep it silent.
	ns := "M\tpkg/model/classifier.go\nM\tpkg/other/thing_test.go\n"
	diff := `--- a/pkg/other/thing_test.go
+++ b/pkg/other/thing_test.go
@@ -10 +9 @@
-	Expect(classify(u)).To(Equal(RepoSource))
`
	if failed, _ := checkTestDilution(context.Background(), "/w", dilutionRunner(ns, diff, nil, nil, nil)); failed {
		t.Fatal("erosion in a package with no production change must not fire (package linkage)")
	}
}

func TestTestDilution_RegisteredAsAdvisory(t *testing.T) {
	var found bool
	var tier gateTier
	for _, c := range gateCheckRegistry("", "", nil) {
		if c.name == "test-dilution" {
			found = true
			tier = c.tier
		}
	}
	if !found {
		t.Fatal(`gateCheckRegistry is missing the "test-dilution" check`)
	}
	if tier != tierAdvisory {
		t.Errorf("test-dilution tier = %v, want tierAdvisory", tier)
	}
}

func TestTestDilution_SurfacesAsAdvisoryNotBlocking(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +20 @@
-	src := "https://huggingface.co/org/model/f.gguf"
+	src := "https://example.com/org/model/f.gguf"
`
	run := dilutionRunner(ns, diff, nil, nil, nil)
	blocking, advisories := runGateChecks(context.Background(), "/w", run,
		[]gateCheck{{name: "test-dilution", tier: tierAdvisory, fn: checkTestDilution}})
	if len(blocking) != 0 {
		t.Errorf("test-dilution must never block; got %d blocking", len(blocking))
	}
	if len(advisories) != 1 || advisories[0].Check != "test-dilution" {
		t.Fatalf("expected one test-dilution advisory; got %+v", advisories)
	}
}

// TestAssertionValueChurn_NestedParens covers the review note on #1416: a
// `[^)]+` regex stops at the first ')', so Equal(f(x), 42) truncated to
// "Equal(f(x)" and a rewrite of the trailing value produced an IDENTICAL
// token on both sides. That is a silent miss, the wrong failure direction
// for a dilution signal. Balanced-paren matching fixes it.
func TestAssertionValueChurn_NestedParens(t *testing.T) {
	got := assertionValueChurn(&fileHunks{
		Removed: []string{`Expect(y).To(Equal(f(x), 42))`},
		Added:   []string{`Expect(y).To(Equal(f(x), 0))`},
	})
	if len(got) == 0 {
		t.Fatalf("expected churn for a value rewrite behind a nested paren, got none")
	}
}

// TestAssertionValueChurn_SpacingIsNotChurn covers the review note that the
// detector's description claimed normalisation it did not perform: a
// gofmt-driven respacing inside the parens read as churn.
func TestAssertionValueChurn_SpacingIsNotChurn(t *testing.T) {
	got := assertionValueChurn(&fileHunks{
		Removed: []string{`Expect(x).To(Equal(42))`},
		Added:   []string{`Expect(x).To(Equal( 42 ))`},
	})
	if len(got) != 0 {
		t.Fatalf("respacing alone must not be churn, got %v", got)
	}
}

// TestAssertionValueChurn_LiteralSpacingIsChurn pins the boundary of the
// normalisation: whitespace OUTSIDE a string literal is noise, whitespace
// INSIDE one is a genuinely different expectation.
func TestAssertionValueChurn_LiteralSpacingIsChurn(t *testing.T) {
	got := assertionValueChurn(&fileHunks{
		Removed: []string{`Expect(x).To(Equal("a b"))`},
		Added:   []string{`Expect(x).To(Equal("a  b"))`},
	})
	if len(got) == 0 {
		t.Fatalf("a changed string literal must still be churn, got none")
	}
}

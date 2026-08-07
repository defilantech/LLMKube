package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// commandStringRunner fakes the three git calls checkCommandStringTestDilution
// makes: `git add -A` (no-op), `git diff --name-status --cached HEAD`, and
// the `git diff --cached --unified=0 ...` full diff.
func commandStringRunner(nameStatus, fullDiff string, nsErr, diffErr error) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		switch {
		case len(args) >= 2 && args[0] == "add" && args[1] == "-A":
			return "", nil
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--name-status":
			return nameStatus, nsErr
		case len(args) >= 2 && args[0] == "diff" && args[1] == "--cached":
			return fullDiff, diffErr
		default:
			return "", nil
		}
	}
}

// TestCheckCommandStringTestDilution_MustFire verifies the must-fire case:
// a production change to a generated shell command whose only added test
// assertions are ContainSubstring on the command string.
func TestCheckCommandStringTestDilution_MustFire(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
+	Expect(cmdStr).To(ContainSubstring("-I"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	failed, out := checkCommandStringTestDilution(context.Background(), "/w", run)
	if !failed {
		t.Fatal("expected advisory when command-string change has only ContainSubstring assertions")
	}
	if !strings.Contains(out, "command-string") || !strings.Contains(out, "string-shape") {
		t.Errorf("detail = %q", out)
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_ParsedResult verifies
// that a command-string change whose added tests assert a parsed result
// (not string-shape) stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_ParsedResult(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(size).To(Equal(int64(42)))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with parsed-result assertion must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_ExitCode verifies
// that a command-string change whose added tests assert an exit code
// stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_ExitCode(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(exitCode).To(Equal(0))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with exit-code assertion must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoTestChange verifies
// that a non-command production change with string assertions stays silent
// because there's no command-string change.
func TestCheckCommandStringTestDilution_MustStaySilent_NoCommandChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	return "hello"
+	return "world"
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(got).To(ContainSubstring("world"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("non-command production change with string assertions must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoTestFileChange verifies
// that any change where no _test.go file changed stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_NoTestFileChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("command-string change with no test file change must stay silent")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_NoProdChange verifies
// that a test-only submission stays silent.
func TestCheckCommandStringTestDilution_MustStaySilent_NoProdChange(t *testing.T) {
	ns := "M\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("test-only submission must stay silent")
	}
}

// TestCheckCommandStringTestDilution_FailOpenOnGitError verifies that a
// git error fails the check open (silent).
func TestCheckCommandStringTestDilution_FailOpenOnGitError(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	if failed, out := checkCommandStringTestDilution(context.Background(), "/w",
		commandStringRunner(ns, diff, nil, errors.New("boom"))); failed || out != "" {
		t.Fatalf("git error must fail open (silent); got failed=%v out=%q", failed, out)
	}
}

// TestCheckCommandStringTestDilution_FailOpenOnNameStatusError verifies that
// a name-status git error fails the check open (silent).
func TestCheckCommandStringTestDilution_FailOpenOnNameStatusError(t *testing.T) {
	if failed, out := checkCommandStringTestDilution(context.Background(), "/w",
		commandStringRunner("", "", errors.New("boom"), nil)); failed || out != "" {
		t.Fatalf("name-status error must fail open (silent); got failed=%v out=%q", failed, out)
	}
}

// TestCheckCommandStringTestDilution_ShellCommandChange fires on "sh", "-c"
// pattern with only string-shape assertions.
func TestCheckCommandStringTestDilution_ShellCommandChange(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("sh", "-c", "curl -I -w '%{size_download}'")
+	cmd := exec.Command("sh", "-c", "curl -I -w '%{size_total}'")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	failed, out := checkCommandStringTestDilution(context.Background(), "/w", run)
	if !failed {
		t.Fatal("expected advisory for shell command change with only string-shape assertions")
	}
	if !strings.Contains(out, "command-string") {
		t.Errorf("detail = %q", out)
	}
}

// TestCheckCommandStringTestDilution_MixedAssertionsStaysSilent verifies
// that when a command-string change has both string-shape and behavioral
// assertions, the check stays silent (not exclusively string-shape).
func TestCheckCommandStringTestDilution_MixedAssertionsStaysSilent(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +22 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
+	Expect(exitCode).To(Equal(0))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := checkCommandStringTestDilution(context.Background(), "/w", run); failed {
		t.Fatal("mixed string-shape and behavioral assertions must stay silent")
	}
}

// TestIsCommandStringLine_ExecCommand verifies the exec.Command( detection.
func TestIsCommandStringLine_ExecCommand(t *testing.T) {
	if !isCommandStringLine("\tcmd := exec.Command(\"curl\", \"-I\")") {
		t.Fatal("exec.Command( should be detected")
	}
}

// TestIsCommandStringLine_ShC verifies the "sh", "-c" detection.
func TestIsCommandStringLine_ShC(t *testing.T) {
	if !isCommandStringLine("\tcmd := exec.Command(\"sh\", \"-c\", \"curl -I\")") {
		t.Fatal("\"sh\", \"-c\" should be detected")
	}
}

// TestIsCommandStringLine_NoMatch verifies non-command lines are not detected.
func TestIsCommandStringLine_NoMatch(t *testing.T) {
	if isCommandStringLine("\treturn \"hello world\"") {
		t.Fatal("plain string return should not be detected as command string")
	}
}

// TestIsStringShapeAssertion_ContainSubstring verifies ContainSubstring is
// classified as string-shape.
func TestIsStringShapeAssertion_ContainSubstring(t *testing.T) {
	if !isStringShapeAssertion("\tExpect(cmdStr).To(ContainSubstring(\"curl\"))") {
		t.Fatal("ContainSubstring should be string-shape")
	}
}

// TestIsStringShapeAssertion_EqualStringLiteral verifies Equal on a string
// literal is classified as string-shape.
func TestIsStringShapeAssertion_EqualStringLiteral(t *testing.T) {
	if !isStringShapeAssertion("\tExpect(got).To(Equal(\"hello\"))") {
		t.Fatal("Equal on string literal should be string-shape")
	}
}

// TestIsStringShapeAssertion_EqualIntNotStringShape verifies Equal on an
// integer is NOT classified as string-shape.
func TestIsStringShapeAssertion_EqualIntNotStringShape(t *testing.T) {
	if isStringShapeAssertion("\tExpect(exitCode).To(Equal(0))") {
		t.Fatal("Equal on integer should not be string-shape")
	}
}

// TestIsStringShapeAssertion_StringsContains verifies strings.Contains is
// classified as string-shape.
func TestIsStringShapeAssertion_StringsContains(t *testing.T) {
	if !isStringShapeAssertion("\tassert.True(t, strings.Contains(cmdStr, \"curl\"))") {
		t.Fatal("strings.Contains should be string-shape")
	}
}

// TestCommandStringTestDilution_RegisteredAsAdvisory verifies the check is
// registered in the gate registry with tierAdvisory.
func TestCommandStringTestDilution_RegisteredAsAdvisory(t *testing.T) {
	var found bool
	var tier gateTier
	for _, c := range gateCheckRegistry("", "", nil) {
		if c.name == "command-string-test-dilution" {
			found = true
			tier = c.tier
		}
	}
	if !found {
		t.Fatal(`gateCheckRegistry is missing the "command-string-test-dilution" check`)
	}
	if tier != tierAdvisory {
		t.Errorf("command-string-test-dilution tier = %v, want tierAdvisory", tier)
	}
}

// TestCommandStringTestDilution_SurfacesAsAdvisoryNotBlocking verifies the
// check never blocks the gate.
func TestCommandStringTestDilution_SurfacesAsAdvisoryNotBlocking(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
`
	run := commandStringRunner(ns, diff, nil, nil)
	blocking, advisories := runGateChecks(context.Background(), "/w", run,
		[]gateCheck{{name: "command-string-test-dilution", tier: tierAdvisory, fn: checkCommandStringTestDilution}})
	if len(blocking) != 0 {
		t.Errorf("command-string-test-dilution must never block; got %d blocking", len(blocking))
	}
	if len(advisories) != 1 || advisories[0].Check != "command-string-test-dilution" {
		t.Fatalf("expected one command-string-test-dilution advisory; got %+v", advisories)
	}
}

// TestCheckCommandStringTestDilution_MustFire_MultilineExecCommand is the
// motivating #1309 shape: a multiline exec.Command where only the changed
// argument appears in the diff. The changed line carries no exec.Command(,
// no "sh"/"-c" pair, and no shellMeta character (% and { are not shell
// metacharacters), so it is detectable only via the enclosing context.
func TestCheckCommandStringTestDilution_MustFire_MultilineExecCommand(t *testing.T) {
	ns := "M\tpkg/model/revalidate.go\nM\tpkg/model/revalidate_test.go\n"
	diff := `--- a/pkg/model/revalidate.go
+++ b/pkg/model/revalidate.go
@@ -7,7 +7,7 @@
 func buildProbe(url string) *exec.Cmd {
 	cmd := exec.Command(
 		"curl",
 		"-I",
-		"-w", "%{size_download}",
+		"-w", "%{size_total}",
 		url,
 	)
--- a/pkg/model/revalidate_test.go
+++ b/pkg/model/revalidate_test.go
@@ -20,3 +20,4 @@
 func TestProbe(t *testing.T) {
+	Expect(cmdStr).To(ContainSubstring("curl"))
 }
`
	run := commandStringRunner(ns, diff, nil, nil)
	failed, out := commandStringGateResult(run)
	if !failed {
		t.Fatal("multiline exec.Command argument change must fire; this is the #1309 case")
	}
	if out == "" {
		t.Error("expected a reviewer detail message")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_UnrelatedStringNearCommand
// guards the enclosing detector against over-firing: a changed line that is
// ordinary code rather than argument-list material must not fire merely
// because an exec.Command sits nearby in the context.
func TestCheckCommandStringTestDilution_MustStaySilent_UnrelatedStringNearCommand(t *testing.T) {
	ns := "M\tpkg/model/revalidate.go\nM\tpkg/model/revalidate_test.go\n"
	diff := `--- a/pkg/model/revalidate.go
+++ b/pkg/model/revalidate.go
@@ -7,7 +7,7 @@
 func buildProbe(url string) *exec.Cmd {
-	label := "old label"
+	label := "new label"
 	cmd := exec.Command(
 		"curl",
 		url,
 	)
--- a/pkg/model/revalidate_test.go
+++ b/pkg/model/revalidate_test.go
@@ -20,3 +20,4 @@
 func TestProbe(t *testing.T) {
+	Expect(cmdStr).To(ContainSubstring("curl"))
 }
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := commandStringGateResult(run); failed {
		t.Fatal("an ordinary assignment near a command must not fire the enclosing detector")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_SiblingBehavioralTestFile
// pins the "every changed test file" quantifier. A package that adds a real
// behavioral assertion in one test file and a shape assertion in another has
// its behavior covered, so the advisory must stay silent.
func TestCheckCommandStringTestDilution_MustStaySilent_SiblingBehavioralTestFile(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/shape_test.go\nM\tpkg/model/behavior_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/shape_test.go
+++ b/pkg/model/shape_test.go
@@ -20 +21 @@
+	Expect(cmdStr).To(ContainSubstring("curl"))
--- a/pkg/model/behavior_test.go
+++ b/pkg/model/behavior_test.go
@@ -30 +31 @@
+	Expect(exitCode).To(Equal(0))
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := commandStringGateResult(run); failed {
		t.Fatal("a sibling test file adding a behavioral assertion must silence the advisory")
	}
}

// TestCheckCommandStringTestDilution_MustStaySilent_QuotesInComment pins the
// Equal( direct-argument check. Counting quotes anywhere on the line reads
// this behavioral assertion as string-shape because of the comment.
func TestCheckCommandStringTestDilution_MustStaySilent_QuotesInComment(t *testing.T) {
	ns := "M\tpkg/model/classifier.go\nM\tpkg/model/classifier_test.go\n"
	diff := `--- a/pkg/model/classifier.go
+++ b/pkg/model/classifier.go
@@ -10 +10 @@
-	cmd := exec.Command("curl", "-I", "-w", "%{size_download}")
+	cmd := exec.Command("curl", "-I", "-w", "%{size_total}")
--- a/pkg/model/classifier_test.go
+++ b/pkg/model/classifier_test.go
@@ -20 +21 @@
+	Expect(exitCode).To(Equal(0)) // the "fast path" returns zero
`
	run := commandStringRunner(ns, diff, nil, nil)
	if failed, _ := commandStringGateResult(run); failed {
		t.Fatal("quotes inside a comment must not turn Equal(0) into a string-shape assertion")
	}
}

// TestIsEqualOnStringLiteral covers the direct-argument rule in isolation.
func TestIsEqualOnStringLiteral(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`Expect(s).To(Equal("draft-simple"))`, true},
		{"Expect(s).To(Equal(`raw`))", true},
		{`Expect(s).To(Equal( "spaced" ))`, true},
		{`Expect(exitCode).To(Equal(0))`, false},
		{`Expect(exitCode).To(Equal(0)) // the "fast path" returns zero`, false},
		{`Expect(n).To(Equal(int64(42)))`, false},
		{`Expect(v).To(BeTrue())`, false},
	}
	for _, tc := range cases {
		if got := isEqualOnStringLiteral(tc.line); got != tc.want {
			t.Errorf("isEqualOnStringLiteral(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestIsStringLiteralArgument covers the argument-shape rule in isolation.
func TestIsStringLiteralArgument(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{`		"-w", "%{size_total}",`, true},
		{`	"curl",`, true},
		{`		"-I",`, true},
		{`	"a", "b")`, true},
		{`	label := "new label"`, false},
		{`	x := f("a") + b`, false},
		{`	cmd := exec.Command("curl")`, false},
		{`	url,`, false},
		{`	// "just a comment"`, false},
	}
	for _, tc := range cases {
		if got := isStringLiteralArgument(tc.line); got != tc.want {
			t.Errorf("isStringLiteralArgument(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

// commandStringGateResult is a thin helper so each case reads as one line.
func commandStringGateResult(run commandRunner) (bool, string) {
	return checkCommandStringTestDilution(context.Background(), "/w", run)
}

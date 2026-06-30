package grounding

import "testing"

func TestFindingString_IncludesFileLineAndMessage(t *testing.T) {
	f := Finding{
		Severity: "blocker", Area: "doc-consistency",
		File: "docs/x.md", Line: 12,
		Message: "unknown API group llmkube.io",
	}
	got := f.String()
	want := "docs/x.md:12 [blocker/doc-consistency] unknown API group llmkube.io"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

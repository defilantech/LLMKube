/*
Copyright 2025.

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

package agent

import (
	"context"
	"strings"
	"testing"
)

func TestHarvestIssueExample_ExtractsFencedBlockNearMarker(t *testing.T) {
	body := "Some intro.\n\n## Example\n```\nUpdate the toolchain image to v2\n```\nmore text\n"
	got := harvestIssueExample(body)
	if !strings.Contains(got, "Update the toolchain image to v2") {
		t.Fatalf("want harvested example, got %q", got)
	}
}

func TestHarvestIssueExample_PrefersMarkedBlock(t *testing.T) {
	body := "```\nfirst unmarked block\n```\n\n### Expected output\n```\nthe expected thing\n```\n"
	got := harvestIssueExample(body)
	if !strings.Contains(got, "the expected thing") {
		t.Fatalf("want the block near the 'Expected' marker, got %q", got)
	}
}

func TestHarvestIssueExample_NoneWhenNoFence(t *testing.T) {
	if got := harvestIssueExample("just prose, no code fence at all"); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestCheckIssueExample_AdvisoryWhenExamplePresent(t *testing.T) {
	fn := checkIssueExample("## Repro\n```\ndo the thing\n```\n")
	failed, out := fn(context.Background(), "/ws", func(context.Context, string, []string, string, ...string) (string, error) { return "", nil })
	if !failed || !strings.Contains(out, "do the thing") {
		t.Fatalf("want advisory containing the example, got failed=%v out=%q", failed, out)
	}
}

func TestCheckIssueExample_NoAdvisoryWhenNoExample(t *testing.T) {
	fn := checkIssueExample("no code blocks here")
	failed, _ := fn(context.Background(), "/ws", func(context.Context, string, []string, string, ...string) (string, error) { return "", nil })
	if failed {
		t.Fatal("no example -> no advisory")
	}
}

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
	"testing"
)

func TestRunGateChecks_SplitsByTier(t *testing.T) {
	block := gateCheck{
		name: "always-block",
		tier: tierBlock,
		fn: func(context.Context, string, commandRunner) (bool, string) {
			return true, "blocked"
		},
	}
	adv := gateCheck{
		name: "always-advise",
		tier: tierAdvisory,
		fn: func(context.Context, string, commandRunner) (bool, string) {
			return true, "noted"
		},
	}
	clean := gateCheck{name: "clean", tier: tierBlock, fn: func(context.Context, string, commandRunner) (bool, string) {
		return false, ""
	}}
	run := func(context.Context, string, []string, string, ...string) (string, error) { return "", nil }

	failures, advisories := runGateChecks(context.Background(), "/ws", run, []gateCheck{block, adv, clean})

	if len(failures) != 1 || failures[0].name != "always-block" {
		t.Fatalf("want 1 blocking failure 'always-block', got %+v", failures)
	}
	if len(advisories) != 1 || advisories[0].Check != "always-advise" || advisories[0].Detail != "noted" {
		t.Fatalf("want 1 advisory 'always-advise', got %+v", advisories)
	}
}

func TestGateCheckEnabled_Toggle(t *testing.T) {
	t.Setenv("FOREMAN_FOO_GATE", "0")
	if gateCheckEnabled("foo") {
		t.Fatal("FOREMAN_FOO_GATE=0 should disable check 'foo'")
	}
	if !gateCheckEnabled("bar") {
		t.Fatal("unset toggle should default enabled")
	}
}

func TestGateCheckRegistry_TiersAndNames(t *testing.T) {
	got := map[string]gateTier{}
	for _, c := range gateCheckRegistry("issue text") {
		if c.fn == nil {
			t.Errorf("check %q has nil fn", c.name)
		}
		got[c.name] = c.tier
	}
	wantBlock := []string{"rbac-use", "import-graph", "embedded-artifact"}
	wantAdvisory := []string{"grounding-breadth", "caller-impact", "issue-example"}
	for _, n := range wantBlock {
		tier, ok := got[n]
		if !ok {
			t.Errorf("missing block check %q", n)
		} else if tier != tierBlock {
			t.Errorf("%s should be block tier, got %v", n, tier)
		}
	}
	for _, n := range wantAdvisory {
		tier, ok := got[n]
		if !ok {
			t.Errorf("missing advisory check %q", n)
		} else if tier != tierAdvisory {
			t.Errorf("%s should be advisory tier, got %v", n, tier)
		}
	}
}
